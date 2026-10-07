package internal

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/sdk-go"
)

// maxSocketLine bounds a single hook event; bufio.Scanner's 64KB default
// silently dropped events carrying a large outputTail.
const maxSocketLine = 1024 * 1024

// socketConnTimeout bounds how long a hook client may keep a connection open.
const socketConnTimeout = 10 * time.Second

// SocketEvent is emitted by shell hooks or agent native hooks.
type SocketEvent struct {
	Type         string `json:"type"`
	AgentID      string `json:"agentId"`
	AgentName    string `json:"agentName"`
	Command      string `json:"command"`
	PID          int    `json:"pid"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	Message      string `json:"message,omitempty"`
	OutputTail   string `json:"outputTail,omitempty"`
	State        string `json:"state,omitempty"`
	Label        string `json:"label,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	Tool         string `json:"tool,omitempty"`
	QuestionText string `json:"questionText,omitempty"`
}

// SocketListener receives lifecycle and status events over a Unix domain socket.
type SocketListener struct {
	path  string
	store *AgentStore
	ln    net.Listener
	done  chan struct{}

	stopOnce sync.Once
}

// NewSocketListener creates a socket listener at the given path.
func NewSocketListener(socketPath string, store *AgentStore) *SocketListener {
	return &SocketListener{
		path:  socketPath,
		store: store,
		done:  make(chan struct{}),
	}
}

// Start binds and begins accepting connections.
func (sl *SocketListener) Start() error {
	dir := filepath.Dir(sl.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	os.Remove(sl.path)
	var err error
	sl.ln, err = net.Listen("unix", sl.path)
	if err != nil {
		return err
	}
	os.Chmod(sl.path, 0600)
	go sl.acceptLoop()
	return nil
}

// Stop closes the listener. Safe to call more than once.
func (sl *SocketListener) Stop() {
	sl.stopOnce.Do(func() {
		close(sl.done)
		if sl.ln != nil {
			sl.ln.Close()
		}
	})
}

func (sl *SocketListener) acceptLoop() {
	for {
		conn, err := sl.ln.Accept()
		if err != nil {
			select {
			case <-sl.done:
				return
			default:
				// Avoid a hot loop on persistent accept errors.
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		go sl.handleConn(conn)
	}
}

func (sl *SocketListener) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(socketConnTimeout))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSocketLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev SocketEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		sl.process(ev)
	}
}

func (sl *SocketListener) process(ev SocketEvent) {
	if ev.AgentID == "" {
		return
	}
	id := sl.store.ResolveHookID(ev.AgentID, ev.PID)
	existing := sl.store.Get(id)
	switch ev.Type {
	case "agent_start":
		if existing == nil {
			sl.store.AddHistory(ev.AgentID, ev.Command, "started", "", time.Now().UnixMilli())
		}
		state := "running"
		source := SourceProc
		if existing != nil && existing.Source == SourceTmux {
			state = OverlayWatcherState(existing.State, "running")
			source = SourceTmux
		}
		sl.store.Upsert(AgentSession{
			ID:          id,
			AgentID:     ev.AgentID,
			DisplayName: ev.AgentName,
			Command:     ev.Command,
			State:       state,
			PID:         ev.PID,
			Source:      source,
		})

	case "agent_end":
		existing := sl.store.Get(id)
		state := "completed"
		if ev.ExitCode != nil && *ev.ExitCode != 0 {
			state = "error"
		}
		if existing == nil {
			// The session was re-keyed to the agent's own PID or already
			// removed; creating a new entry here left a nameless ghost row.
			return
		}
		sl.store.AddHistory(existing.AgentID, existing.Command, state, existing.Task, time.Now().UnixMilli())
		source := SourceProc
		if existing.Source == SourceTmux {
			source = SourceTmux
		}
		sl.store.Upsert(AgentSession{
			ID:       id,
			State:    state,
			PID:      ev.PID,
			ExitCode: ev.ExitCode,
			Source:   source,
		})
		sdk.Log("agent %s ended with state=%s", ev.AgentID, state)

	case "agent_question":
		state := "question"
		source := SourceProc
		if existing != nil && existing.Source == SourceTmux {
			state = OverlayWatcherState(existing.State, "question")
			source = SourceTmux
		}
		sl.store.Upsert(AgentSession{
			ID:           id,
			AgentID:      ev.AgentID,
			DisplayName:  ev.AgentName,
			State:        state,
			Task:         ev.Message,
			QuestionText: ev.QuestionText,
			PID:          ev.PID,
			Source:       source,
		})
		sdk.Log("agent %s needs attention", ev.AgentID)

	case "agent_status":
		state := ev.State
		if state == "" {
			state = "running"
		}
		source := SourceProc
		if existing != nil && existing.Source == SourceTmux {
			state = OverlayWatcherState(existing.State, state)
			source = SourceTmux
		}
		sl.store.Upsert(AgentSession{
			ID:           id,
			AgentID:      ev.AgentID,
			DisplayName:  ev.AgentName,
			Command:      ev.Command,
			State:        state,
			Task:         ev.Message,
			QuestionText: ev.QuestionText,
			CurrentTool:  ev.Tool,
			SessionID:    ev.SessionID,
			PID:          ev.PID,
			Source:       source,
		})
		sdk.Log("agent %s status: %s", ev.AgentID, state)
	}
}
