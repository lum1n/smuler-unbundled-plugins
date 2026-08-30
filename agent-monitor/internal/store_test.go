package internal

import (
	"testing"
	"time"
)

func TestAgentStore_UpsertMergesPartialUpdates(t *testing.T) {
	s := NewAgentStore()

	s.Upsert(AgentSession{
		ID:          "claude-123",
		AgentID:     "claude",
		DisplayName: "Claude Code",
		Command:     "claude",
		State:       "running",
		PID:         123,
	})

	s.Upsert(AgentSession{
		ID:          "claude-123",
		State:       "working",
		CurrentTool: "Read",
		CurrentFile: "/tmp/foo.ts",
		Model:       "claude-sonnet-4",
	})

	got := s.Get("claude-123")
	if got == nil {
		t.Fatal("expected session")
	}
	if got.State != "working" {
		t.Errorf("state = %q, want working", got.State)
	}
	if got.Command != "claude" {
		t.Errorf("command = %q, want claude", got.Command)
	}
	if got.CurrentTool != "Read" {
		t.Errorf("currentTool = %q, want Read", got.CurrentTool)
	}
	if got.Model != "claude-sonnet-4" {
		t.Errorf("model = %q, want claude-sonnet-4", got.Model)
	}
	if got.PID != 123 {
		t.Errorf("pid = %d, want 123", got.PID)
	}
}

func TestAgentStore_TerminalState(t *testing.T) {
	s := NewAgentStore()
	s.Upsert(AgentSession{ID: "pi-1", AgentID: "pi", State: "running"})

	code := 1
	s.Upsert(AgentSession{ID: "pi-1", State: "error", ExitCode: &code})

	got := s.Get("pi-1")
	if !got.IsTerminalState() {
		t.Error("expected terminal state")
	}
	if *got.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", *got.ExitCode)
	}
}

func TestAgentStore_RemoveCompletedOlderThan(t *testing.T) {
	s := NewAgentStore()
	now := time.Now().UnixMilli()
	s.Upsert(AgentSession{ID: "a", AgentID: "pi", State: "completed", UpdatedAt: now - 120000})
	s.Upsert(AgentSession{ID: "b", AgentID: "pi", State: "running", UpdatedAt: now})

	s.RemoveCompletedOlderThan(now - 60000)

	if s.Get("a") != nil {
		t.Error("expected old completed session to be removed")
	}
	if s.Get("b") == nil {
		t.Error("expected running session to remain")
	}
}

func TestAgentStore_WatcherOwnsPID(t *testing.T) {
	s := NewAgentStore()
	s.Upsert(AgentSession{ID: "tmux:dev:w0", AgentID: "claude", PID: 7, State: "idle", Source: SourceTmux})
	s.Upsert(AgentSession{ID: "claude-9", AgentID: "claude", PID: 9, State: "running", Source: SourceProc})
	if !s.WatcherOwnsPID(7) {
		t.Fatal("expected watcher to own pane pid")
	}
	if s.WatcherOwnsPID(9) {
		t.Fatal("proc pid is not watcher-owned")
	}
	if s.ResolveHookID("claude", 7) != "tmux:dev:w0" {
		t.Fatal(s.ResolveHookID("claude", 7))
	}
}

func TestAgentStore_HistoryLimit(t *testing.T) {
	s := NewAgentStore()
	for i := 0; i < 60; i++ {
		s.AddHistory("pi", "pi", "completed", "task", int64(i))
	}
	if len(s.History()) != 50 {
		t.Errorf("history len = %d, want 50", len(s.History()))
	}
}
