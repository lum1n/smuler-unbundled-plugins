package internal

import (
	"testing"
	"time"
)

func TestProcDetectorPreservesWorkingState(t *testing.T) {
	store := NewAgentStore()
	pd := NewProcDetector(store, time.Hour)

	id := "claude-4242"
	store.Upsert(AgentSession{
		ID:      id,
		AgentID: "claude",
		State:   "working",
		PID:     4242,
	})

	seen := make(map[string]bool)
	pd.matchAndRecord([]string{"claude", "chat"}, 4242, 0, seen)

	got := store.Get(id)
	if got == nil {
		t.Fatal("expected session")
	}
	if got.State != "working" {
		t.Fatalf("state = %q, want working", got.State)
	}
}

func TestProcDetectorRekeysShellHookSession(t *testing.T) {
	store := NewAgentStore()
	pd := NewProcDetector(store, time.Hour)

	store.Upsert(AgentSession{
		ID:      "claude-1000",
		AgentID: "claude",
		State:   "thinking",
		PID:     1000,
	})

	seen := make(map[string]bool)
	pd.matchAndRecord([]string{"claude", "chat"}, 4242, 1000, seen)

	if store.Get("claude-1000") != nil {
		t.Fatal("expected shell-hook session to be re-keyed")
	}
	got := store.Get("claude-4242")
	if got == nil {
		t.Fatal("expected re-keyed session")
	}
	if got.State != "thinking" {
		t.Fatalf("state = %q, want thinking", got.State)
	}
	if got.PID != 4242 {
		t.Fatalf("pid = %d, want 4242", got.PID)
	}
}

func TestRemoveOrphansKeepsWatcherSessions(t *testing.T) {
	store := NewAgentStore()
	pd := NewProcDetector(store, time.Hour)
	stale := time.Now().Add(-2 * orphanGrace).UnixMilli()

	store.Upsert(AgentSession{ID: "tmux-pane", AgentID: "claude", State: "working", Source: SourceTmux, UpdatedAt: stale})
	store.Upsert(AgentSession{ID: "hook-ghost", AgentID: "claude", State: "working", Source: SourceProc, UpdatedAt: stale})

	pd.removeOrphans(map[string]bool{})

	if store.Get("tmux-pane") == nil {
		t.Fatal("watcher-owned tmux session was pruned")
	}
	if store.Get("hook-ghost") != nil {
		t.Fatal("orphaned hook session without a PID was kept")
	}
}
