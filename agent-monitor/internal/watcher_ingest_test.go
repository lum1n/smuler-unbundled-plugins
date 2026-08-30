package internal

import (
	"encoding/json"
	"testing"

	"github.com/lum1n/smuler/plugins/agent-monitor/internal/watcher"
)

func TestIngestSnapshotMapsAndResolvesPane(t *testing.T) {
	store := NewAgentStore()
	line := `{"v":1,"type":"snapshot","agents":[{"session":"api","window":1,"kind":"claude","state":"waiting-permission","summary":"Allow edit?"}]}`
	var ev watcher.Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatal(err)
	}

	lookup := func(session string, window int) (int, string, error) {
		if session != "api" || window != 1 {
			t.Fatalf("lookup %s:%d", session, window)
		}
		return 4242, "/tmp/repo", nil
	}
	IngestWatcherEvent(store, ev, lookup)

	got := store.Get("tmux:api:w1")
	if got == nil {
		t.Fatal("missing session")
	}
	if got.State != "question" {
		t.Fatalf("state = %q", got.State)
	}
	if got.PID != 4242 || got.CWD != "/tmp/repo" {
		t.Fatalf("pid/cwd = %d %q", got.PID, got.CWD)
	}
	if got.Source != SourceTmux {
		t.Fatalf("source = %q", got.Source)
	}
	if got.QuestionText != "Allow edit?" {
		t.Fatalf("question = %q", got.QuestionText)
	}
}

func TestIngestGoneMarksCompleted(t *testing.T) {
	store := NewAgentStore()
	store.Upsert(AgentSession{ID: "tmux:api:w0", AgentID: "pi", State: "idle", Source: SourceTmux})
	IngestWatcherEvent(store, watcher.Event{Type: "gone", Session: "api", Window: 0}, nil)
	got := store.Get("tmux:api:w0")
	if got == nil || got.State != "completed" {
		t.Fatalf("%+v", got)
	}
}

func TestIngestUnboundIsIdleNoAlert(t *testing.T) {
	store := NewAgentStore()
	IngestWatcherEvent(store, watcher.Event{
		Type:    "unbound",
		Session: "api",
		Window:  3,
		Kind:    "opencode",
	}, nil)
	got := store.Get("tmux:api:w3")
	if got == nil {
		t.Fatal("missing")
	}
	if got.State != "idle" {
		t.Fatalf("state = %q", got.State)
	}
	if watcher.EmitsEvent(got.State) != "" {
		t.Fatal("unbound must not emit")
	}
}

func TestProcDetectorSkipsWatcherPID(t *testing.T) {
	store := NewAgentStore()
	store.Upsert(AgentSession{
		ID:     "tmux:api:w0",
		AgentID: "claude",
		PID:    4242,
		State:  "thinking",
		Source: SourceTmux,
	})
	pd := NewProcDetector(store, 0)
	seen := map[string]bool{}
	pd.matchAndRecord([]string{"claude", "chat"}, 4242, 1, seen)
	if store.Get("claude-4242") != nil {
		t.Fatal("expected proc row to be skipped")
	}
	if store.Get("tmux:api:w0") == nil {
		t.Fatal("watcher row must remain")
	}
}

func TestProcDetectorRecordsNonTmux(t *testing.T) {
	store := NewAgentStore()
	pd := NewProcDetector(store, 0)
	seen := map[string]bool{}
	pd.matchAndRecord([]string{"claude", "chat"}, 99, 1, seen)
	got := store.Get("claude-99")
	if got == nil {
		t.Fatal("expected proc session")
	}
	if got.Source != SourceProc {
		t.Fatalf("source = %q", got.Source)
	}
}

func TestOverlayWatcherState(t *testing.T) {
	if OverlayWatcherState("thinking", "question") != "thinking" {
		t.Fatal("watcher thinking must win")
	}
	if OverlayWatcherState("idle", "question") != "question" {
		t.Fatal("hook question may beat idle")
	}
	if OverlayWatcherState("working", "idle") != "working" {
		t.Fatal("watcher working must win")
	}
}
