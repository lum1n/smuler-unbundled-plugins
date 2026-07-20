package internal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPiContextReader_ReadsSession(t *testing.T) {
	tmp := t.TempDir()
	cwd := "/home/user/projects/acme"
	escaped := escapePiCWD(cwd)
	sessionsDir := filepath.Join(tmp, "--"+escaped+"--")
	if err := os.MkdirAll(sessionsDir, 0700); err != nil {
		t.Fatal(err)
	}

	sessionPath := filepath.Join(sessionsDir, "20260115_123456_abcdef.jsonl")
	data := []byte(`{"type":"session","version":3,"id":"sess-abc","timestamp":"2026-01-15T12:34:56.000Z","cwd":"/home/user/projects/acme"}
{"type":"session_info","id":"si1","parentId":"sess-abc","timestamp":"2026-01-15T12:34:57.000Z","name":"Refactor auth"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-01-15T12:34:58.000Z","message":{"role":"user","content":"Refactor the auth module","timestamp":1736945698000}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-01-15T12:35:00.000Z","message":{"role":"assistant","model":"claude-sonnet-4","provider":"anthropic","content":[{"type":"text","text":"I'll refactor auth"}],"usage":{"input":100,"output":50,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.0025}},"timestamp":1736945700000}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-01-15T12:35:05.000Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"Read","arguments":{"file_path":"/home/user/projects/acme/auth.ts"}}],"timestamp":1736945705000}}
{"type":"message","id":"m4","parentId":"m3","timestamp":"2026-01-15T12:35:06.000Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"Read","content":[{"type":"text","text":"export function login() {}"}],"timestamp":1736945706000}}
`)
	if err := os.WriteFile(sessionPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	reader := NewPiContextReaderAt(tmp)
	ctx := reader.readSession(sessionPath, cwd)

	if !ctx.Available {
		t.Fatal("expected available context")
	}
	if ctx.AgentID != "pi" {
		t.Errorf("agentID = %q, want pi", ctx.AgentID)
	}
	if ctx.DisplayName != "Pi" {
		t.Errorf("displayName = %q, want Pi", ctx.DisplayName)
	}
	if ctx.Task != "Refactor auth" {
		t.Errorf("task = %q, want Refactor auth", ctx.Task)
	}
	if ctx.CurrentTool != "Read" {
		t.Errorf("currentTool = %q, want Read", ctx.CurrentTool)
	}
	if ctx.CurrentFile != "/home/user/projects/acme/auth.ts" {
		t.Errorf("currentFile = %q", ctx.CurrentFile)
	}
	if ctx.Model != "claude-sonnet-4" {
		t.Errorf("model = %q, want claude-sonnet-4", ctx.Model)
	}
	if ctx.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", ctx.Provider)
	}
	if ctx.TokensInput != 100 {
		t.Errorf("tokensInput = %d, want 100", ctx.TokensInput)
	}
	if ctx.TokensOutput != 50 {
		t.Errorf("tokensOutput = %d, want 50", ctx.TokensOutput)
	}
	if ctx.Cost != 0.0025 {
		t.Errorf("cost = %v, want 0.0025", ctx.Cost)
	}
}

func TestPiContextReader_NoSession(t *testing.T) {
	tmp := t.TempDir()
	reader := NewPiContextReaderAt(tmp)
	ctx := reader.ContextForPID(0)
	if ctx.Available {
		t.Error("expected no context for pid 0")
	}
}
