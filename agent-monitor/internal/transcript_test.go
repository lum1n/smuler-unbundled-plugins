package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestJSONLTailIncrementalPartialAndHugeLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	huge := `{"x":"` + strings.Repeat("a", 3*1024*1024) + `"}`
	appendFile(t, path, "{\"n\":1}\n"+huge+"\n{\"n\":")

	var tail jsonlTail
	var lines []string
	resets := 0
	read := func() {
		if err := tail.next(path, func() { resets++; lines = nil }, func(l []byte) { lines = append(lines, string(l)) }); err != nil {
			t.Fatal(err)
		}
	}

	read()
	if len(lines) != 2 || len(lines[1]) != len(huge) {
		t.Fatalf("expected 2 complete lines incl. huge one, got %d", len(lines))
	}

	// Completing the partial line delivers it exactly once.
	appendFile(t, path, "2}\n")
	read()
	if len(lines) != 3 || lines[2] != `{"n":2}` {
		t.Fatalf("lines = %v", lines[2:])
	}
	read()
	if len(lines) != 3 {
		t.Fatalf("unchanged file re-delivered lines: %d", len(lines))
	}

	// Truncation resets.
	if err := os.WriteFile(path, []byte("{\"n\":9}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	read()
	if resets != 1 || len(lines) != 1 || lines[0] != `{"n":9}` {
		t.Fatalf("resets=%d lines=%v", resets, lines)
	}
}

func TestClaudeReaderIncrementalState(t *testing.T) {
	base := t.TempDir()
	cwd := "/Users/me/src/my.repo"
	projDir := filepath.Join(base, "projects", claudeProjectDirName(cwd))
	if err := os.MkdirAll(projDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "sessions", "4242.json"),
		[]byte(`{"pid":4242,"sessionId":"sess-1","cwd":"`+cwd+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projDir, "sess-1.jsonl")
	appendFile(t, transcript, `{"type":"user","message":{"role":"user","content":"Fix the login bug"}}
{"type":"assistant","message":{"role":"assistant","model":"claude-x","content":[{"type":"tool_use","name":"TodoWrite","input":{"todos":[{"content":"step one","status":"pending"}]}}],"usage":{"input_tokens":10,"output_tokens":5}}}
`)

	r := NewClaudeContextReaderAt(base)
	meta, ok := r.sessionMetaForPID(4242)
	if !ok {
		t.Fatal("expected session meta")
	}
	if got := r.findTranscript(meta.SessionID, meta.CWD); got != transcript {
		t.Fatalf("transcript = %q", got)
	}

	st := r.transcriptState(transcript)
	if st.lastPrompt != "Fix the login bug" {
		t.Errorf("lastPrompt = %q", st.lastPrompt)
	}
	if len(st.todos) != 1 || st.todos[0].Content != "step one" {
		t.Errorf("todos = %+v", st.todos)
	}
	if !st.pendingToolUse || st.model != "claude-x" {
		t.Errorf("state = %+v", st)
	}

	// Tool result (user role, no text) must not replace the prompt.
	appendFile(t, transcript, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All fixed."}]}}
`)
	st = r.transcriptState(transcript)
	if st.lastPrompt != "Fix the login bug" {
		t.Errorf("lastPrompt after tool result = %q", st.lastPrompt)
	}
	state, _ := InferSessionState(SessionSignals{
		LastRole:          st.lastRole,
		LastAssistantText: st.lastAssistantText,
		CurrentTool:       st.currentTool,
		PendingToolUse:    st.pendingToolUse,
	})
	if state != "running" {
		t.Errorf("finished turn state = %q, want running", state)
	}
}

func TestCodexRolloutParsing(t *testing.T) {
	lines := []string{
		`{"type":"turn_context","payload":{"model":"gpt-5-codex"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>...</environment_context>"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Add retries"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{}"}}`,
	}
	var st codexRolloutState
	for _, l := range lines {
		updateCodexStateFromLine([]byte(l), &st)
	}
	if st.task != "Add retries" || st.model != "gpt-5-codex" || st.currentTool != "shell" || !st.pendingToolUse {
		t.Fatalf("state = %+v", st)
	}
	updateCodexStateFromLine([]byte(`{"type":"response_item","payload":{"type":"function_call_output","output":"ok"}}`), &st)
	updateCodexStateFromLine([]byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":20}}}}`), &st)
	updateCodexStateFromLine([]byte(`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Done."}}`), &st)
	if st.task != "Add retries" {
		t.Errorf("task overwritten: %q", st.task)
	}
	if st.tokensInput != 100 || st.tokensOutput != 20 {
		t.Errorf("tokens = %d/%d", st.tokensInput, st.tokensOutput)
	}
	state, _ := InferSessionState(SessionSignals{LastRole: st.lastRole, LastAssistantText: st.lastText, CurrentTool: st.currentTool, PendingToolUse: st.pendingToolUse})
	if state != "running" {
		t.Errorf("state = %q, want running", state)
	}
}

func TestPiReaderUsesLatestBranchValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendFile(t, path, `{"type":"session","id":"sess","cwd":"/x"}
{"type":"message","id":"m1","timestamp":"2026-01-01T00:00:01.000Z","message":{"role":"user","content":"first prompt"}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-01T00:00:02.000Z","message":{"role":"assistant","model":"old-model","content":[{"type":"text","text":"ok"}],"usage":{"input":1,"output":1}}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-01-01T00:00:03.000Z","message":{"role":"user","content":"second prompt"}}
{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-01-01T00:00:04.000Z","message":{"role":"assistant","model":"new-model","content":[{"type":"text","text":"done"}],"usage":{"input":2,"output":2}}}
`)
	r := NewPiContextReaderAt(t.TempDir())
	ctx := r.readSession(path, "/x")
	if ctx.Task != "second prompt" || ctx.Model != "new-model" {
		t.Fatalf("task=%q model=%q", ctx.Task, ctx.Model)
	}
	// Re-reading an unchanged file must not double-count usage.
	ctx = r.readSession(path, "/x")
	if ctx.TokensInput != 3 || ctx.TokensOutput != 3 {
		t.Fatalf("tokens = %d/%d, want 3/3", ctx.TokensInput, ctx.TokensOutput)
	}
}

func TestSocketAgentEndForUnknownSessionIsIgnored(t *testing.T) {
	store := NewAgentStore()
	sl := NewSocketListener(filepath.Join(t.TempDir(), "s.sock"), store)
	code := 0
	sl.process(SocketEvent{Type: "agent_end", AgentID: "claude", PID: 99, ExitCode: &code})
	if len(store.List()) != 0 {
		t.Fatalf("agent_end created a ghost session: %+v", store.List())
	}
}
