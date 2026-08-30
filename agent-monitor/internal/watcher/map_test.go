package watcher

import (
	"os"
	"testing"
)

func TestMapState(t *testing.T) {
	tests := []struct {
		in      string
		unbound bool
		want    string
	}{
		{"waiting-permission", false, "question"},
		{"running-tool", false, "working"},
		{"thinking", false, "thinking"},
		{"idle", false, "idle"},
		{"", false, "idle"},
		{"errored", false, "error"},
		{"thinking", true, "idle"},
		{"waiting-permission", true, "idle"},
	}
	for _, tt := range tests {
		if got := MapState(tt.in, tt.unbound); got != tt.want {
			t.Errorf("MapState(%q, %v) = %q, want %q", tt.in, tt.unbound, got, tt.want)
		}
	}
}

func TestSessionKey(t *testing.T) {
	if got := SessionKey("dev", 2); got != "tmux:dev:w2" {
		t.Fatalf("SessionKey = %q", got)
	}
}

func TestDisplayName(t *testing.T) {
	if DisplayName("cursor") != "Cursor" {
		t.Fatal(DisplayName("cursor"))
	}
	if DisplayName("claude") != "Claude Code" {
		t.Fatal(DisplayName("claude"))
	}
}

func TestResolveScriptFindsThirdParty(t *testing.T) {
	dir := t.TempDir()
	src := dir + "/third_party/agent-watcher/src"
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	path := src + "/agent_watcher.py"
	if err := os.WriteFile(path, []byte("#"), 0644); err != nil {
		t.Fatal(err)
	}
	got := ResolveScript(dir, "")
	if got != path {
		t.Fatalf("got %q want %q", got, path)
	}
}

func TestEmitsEvent(t *testing.T) {
	if EmitsEvent("question") != "agent_question" {
		t.Fatal("want agent_question")
	}
	if EmitsEvent("idle") != "" {
		t.Fatal("idle should not emit")
	}
}
