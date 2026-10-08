package internal

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcDetector scans the process table for known agent executables.
type ProcDetector struct {
	store    *AgentStore
	execs    map[string]string
	interval time.Duration
	done     chan struct{}
	tracked  map[string]bool

	startOnce sync.Once
	stopOnce  sync.Once
}

// NewProcDetector creates a process detector with the default agent registry.
func NewProcDetector(store *AgentStore, interval time.Duration) *ProcDetector {
	return &ProcDetector{
		store: store,
		execs: map[string]string{
			"archer":       "archer",
			"claude":       "claude",
			"opencode":     "opencode",
			"codex":        "codex",
			"pi":           "pi",
			"aider":        "aider",
			"cursor":       "cursor",
			"cursor-agent": "cursor",
		},
		interval: interval,
		done:     make(chan struct{}),
		tracked:  make(map[string]bool),
	}
}

// SetInterval changes the polling interval; it only takes effect before Start.
func (pd *ProcDetector) SetInterval(d time.Duration) {
	if d > 0 {
		pd.interval = d
	}
}

// Start begins the polling loop. Repeated calls are no-ops.
func (pd *ProcDetector) Start() { pd.startOnce.Do(func() { go pd.pollLoop() }) }

// Stop terminates the polling loop. Safe to call more than once.
func (pd *ProcDetector) Stop() { pd.stopOnce.Do(func() { close(pd.done) }) }

// orphanGrace is how long a hook-created session whose process has exited
// is kept before it is dropped.
const orphanGrace = 30 * time.Second

func (pd *ProcDetector) pollLoop() {
	ticker := time.NewTicker(pd.interval)
	defer ticker.Stop()
	pd.scan()
	for {
		select {
		case <-ticker.C:
			pd.scan()
		case <-pd.done:
			return
		}
	}
}

func (pd *ProcDetector) scan() {
	seen := make(map[string]bool)

	switch runtime.GOOS {
	case "darwin":
		pd.scanPS(seen)
	case "linux":
		pd.scanProc(seen)
	}

	for id := range pd.tracked {
		if !seen[id] {
			entry := pd.store.Get(id)
			if entry != nil && entry.IsTerminalState() {
				continue
			}
			pd.store.Remove(id)
			delete(pd.tracked, id)
		}
	}

	pd.removeOrphans(seen)
	pd.store.RemoveCompletedOlderThan(time.Now().UnixMilli() - 60000)
}

// removeOrphans drops non-terminal sessions created by hooks (never matched
// by the process scan) whose process is gone. Without this they stayed in
// the store, and the menubar, forever.
func (pd *ProcDetector) removeOrphans(seen map[string]bool) {
	cutoff := time.Now().Add(-orphanGrace).UnixMilli()
	for _, a := range pd.store.List() {
		if seen[a.ID] || pd.tracked[a.ID] || a.IsTerminalState() || a.UpdatedAt > cutoff {
			continue
		}
		// tmux sessions are owned by the agent-watcher, which reports their
		// lifecycle (and may have no PID for unbound panes).
		if a.Source == SourceTmux {
			continue
		}
		if a.PID <= 0 || !processAlive(a.PID) {
			pd.store.Remove(a.ID)
		}
	}
}

func (pd *ProcDetector) matchAndRecord(args []string, pid int, ppid int, seen map[string]bool) {
	if len(args) == 0 || args[0] == "" {
		return
	}
	agentID, ok := pd.execs[strings.ToLower(filepath.Base(args[0]))]
	if !ok {
		return
	}
	if pd.store.WatcherOwnsPID(pid) || pd.store.WatcherOwnsPID(ppid) {
		return
	}
	if existing := pd.store.FindByPID(pid); existing != nil && existing.Source == SourceTmux {
		return
	}
	id := agentID + "-" + strconv.Itoa(pid)

	existing := pd.store.Get(id)
	if existing != nil {
		if existing.IsTerminalState() {
			return
		}
		// Preserve hook/reader-derived state; only refresh PID if missing.
		if existing.PID == 0 {
			pd.store.Upsert(AgentSession{ID: id, AgentID: agentID, PID: pid})
		}
		seen[id] = true
		pd.tracked[id] = true
		return
	}

	if ppid > 0 {
		hookID := agentID + "-" + strconv.Itoa(ppid)
		if hookEntry := pd.store.Get(hookID); hookEntry != nil && !hookEntry.IsTerminalState() {
			// Re-key shell-hook session to the real agent PID without resetting state.
			if hookEntry.ID != id {
				pd.store.Upsert(AgentSession{
					ID:           id,
					AgentID:      agentID,
					DisplayName:  hookEntry.DisplayName,
					Command:      hookEntry.Command,
					State:        hookEntry.State,
					Task:         hookEntry.Task,
					QuestionText: hookEntry.QuestionText,
					CurrentTool:  hookEntry.CurrentTool,
					SessionID:    hookEntry.SessionID,
					PID:          pid,
				})
				pd.store.Remove(hookEntry.ID)
			}
			seen[id] = true
			pd.tracked[id] = true
			return
		}
	}

	seen[id] = true
	pd.tracked[id] = true
	pd.store.Upsert(AgentSession{
		ID:      id,
		AgentID: agentID,
		Command: strings.Join(args, " "),
		State:   "running",
		PID:     pid,
		Source:  SourceProc,
	})
}

func (pd *ProcDetector) scanPS(seen map[string]bool) {
	out, err := commandOutput("ps", "-Ao", "pid=,ppid=,args=")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			ppid = 0
		}
		pd.matchAndRecord(fields[2:], pid, ppid, seen)
	}
}

func (pd *ProcDetector) scanProc(seen map[string]bool) {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		ppid := 0
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err == nil {
			fields := strings.Fields(string(stat))
			if len(fields) > 3 {
				ppid, _ = strconv.Atoi(fields[3])
			}
		}
		pd.matchAndRecord(args, pid, ppid, seen)
	}
}
