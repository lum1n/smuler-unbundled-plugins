package internal

import "testing"

func TestInferSessionState(t *testing.T) {
	tests := []struct {
		name     string
		signals  SessionSignals
		want     string
		wantQ    string
	}{
		{
			name:    "user prompt in flight",
			signals: SessionSignals{LastRole: "user"},
			want:    "working",
		},
		{
			name:    "pending tool use",
			signals: SessionSignals{LastRole: "assistant", PendingToolUse: true},
			want:    "working",
		},
		{
			name:    "active tool execution",
			signals: SessionSignals{ActiveToolExec: true},
			want:    "working",
		},
		{
			name:    "current tool without user turn",
			signals: SessionSignals{LastRole: "assistant", CurrentTool: "Bash"},
			want:    "working",
		},
		{
			name:    "assistant question",
			signals: SessionSignals{LastRole: "assistant", LastAssistantText: "Should I continue?"},
			want:    "question",
			wantQ:   "Should I continue?",
		},
		{
			name:    "assistant idle between turns",
			signals: SessionSignals{LastRole: "assistant", LastAssistantText: "Done."},
			want:    "thinking",
		},
		{
			name:    "no signals",
			signals: SessionSignals{},
			want:    "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotQ := InferSessionState(tt.signals)
			if got != tt.want {
				t.Fatalf("state = %q, want %q", got, tt.want)
			}
			if gotQ != tt.wantQ {
				t.Fatalf("question = %q, want %q", gotQ, tt.wantQ)
			}
		})
	}
}

func TestMergeAgentState(t *testing.T) {
	if got := MergeAgentState("working", "running"); got != "working" {
		t.Fatalf("got %q, want working", got)
	}
	if got := MergeAgentState("running", "working"); got != "working" {
		t.Fatalf("got %q, want working", got)
	}
	if got := MergeAgentState("thinking", "working"); got != "working" {
		t.Fatalf("got %q, want working", got)
	}
	if got := MergeAgentState("question", "working"); got != "question" {
		t.Fatalf("got %q, want question", got)
	}
}

func TestOverlayWatcherStateBeatsStaleIdle(t *testing.T) {
	if got := OverlayWatcherState("idle", "question"); got != "question" {
		t.Fatalf("got %q", got)
	}
	if got := OverlayWatcherState("unbound", "question"); got != "question" {
		t.Fatalf("got %q", got)
	}
	if got := OverlayWatcherState("thinking", "working"); got != "thinking" {
		t.Fatalf("got %q, want thinking (watcher wins)", got)
	}
}
