package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/lum1n/smuler/plugins/agent-monitor/internal"
)

func TestGetStatusEnrichesConcurrentlyWithUniqueIDs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	for _, p := range []string{".claude/skills/a/SKILL.md", ".claude/skills/b/SKILL.md", "AGENTS.md"} {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# "+p+"\nbody\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	h := newHandler()
	setup := h.setupItems([]internal.AgentSession{{RepoRoot: repo}})
	ids := map[string]bool{}
	for _, it := range setup {
		if ids[it.ID] {
			t.Fatalf("duplicate setup item id %q", it.ID)
		}
		ids[it.ID] = true
	}
	if len(setup) != 4 { // header + 3 files
		t.Fatalf("setup items = %d, want 4", len(setup))
	}

	pid := os.Getpid()
	for i, agent := range []string{"claude", "codex", "pi", "opencode", "aider"} {
		h.store.Upsert(internal.AgentSession{
			ID:      agent + "-" + strconv.Itoa(i),
			AgentID: agent,
			PID:     pid,
			State:   "running",
			CWD:     repo,
		})
	}

	snap := h.GetStatus()
	seen := map[string]bool{}
	for _, it := range snap.Items {
		if seen[it.ID] {
			t.Fatalf("duplicate item id %q", it.ID)
		}
		seen[it.ID] = true
		switch it.Severity {
		case "info", "warning", "critical":
		default:
			t.Fatalf("invalid severity %q on %s", it.Severity, it.ID)
		}
	}
	if snap.RefreshAfter <= 0 {
		t.Fatalf("refreshAfter = %d", snap.RefreshAfter)
	}
}

func TestSignalAgentReportsFailure(t *testing.T) {
	cmd := exec.Command("sleep", "0")
	if err := cmd.Run(); err != nil {
		t.Skip("sleep unavailable")
	}
	// The exited child's pid is reaped, so signalling it must fail.
	if ok, _ := signalAgent(cmd.Process.Pid, 0); ok {
		t.Skip("pid was reused")
	}
	if ok, _ := signalAgent(0, 0); ok {
		t.Fatal("expected failure for pid 0")
	}
}
