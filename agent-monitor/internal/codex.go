package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CodexContextReader reads Codex thread metadata and rollout logs.
type CodexContextReader struct {
	home string
	db   string
}

type codexThreadRow struct {
	ID         string `json:"id"`
	Rollout    string `json:"rollout_path"`
	CWD        string `json:"cwd"`
	Title      string `json:"title"`
	GitBranch  string `json:"git_branch"`
	Archived   int    `json:"archived"`
	UpdatedAt  int64  `json:"updated_at"`
}

// NewCodexContextReader creates a reader for Codex's local SQLite + rollout logs.
func NewCodexContextReader() *CodexContextReader {
	home, _ := os.UserHomeDir()
	return &CodexContextReader{
		home: home,
		db:   filepath.Join(home, ".codex", "logs_2.sqlite"),
	}
}

// ContextForPID returns Codex session context for the given process.
func (r *CodexContextReader) ContextForPID(pid int) AgentSession {
	if r == nil || pid <= 0 {
		return AgentSession{}
	}
	if _, err := os.Stat(r.db); err != nil {
		return AgentSession{}
	}
	cwd := cwdForPID(pid)
	if cwd == "" {
		return AgentSession{}
	}

	rows, err := sqliteQueryJSON[codexThreadRow](
		r.db,
		fmt.Sprintf(
			"SELECT id, rollout_path, cwd, title, git_branch, archived, updated_at FROM threads WHERE cwd=%s AND archived=0 ORDER BY updated_at DESC LIMIT 1",
			sqliteQuote(cwd),
		),
	)
	if err != nil || len(rows) == 0 {
		return AgentSession{}
	}
	thread := rows[0]
	ctx := AgentSession{
		AgentID:     "codex",
		DisplayName: "Codex",
		SessionID:   thread.ID,
		Task:        thread.Title,
		CWD:         thread.CWD,
		Branch:      thread.GitBranch,
		Available:   true,
	}

	rollout := r.resolveRolloutPath(thread.Rollout)
	if rollout != "" {
		state := readCodexRolloutState(rollout)
		if state.task != "" {
			ctx.Task = state.task
		}
		ctx.CurrentTool = state.currentTool
		ctx.CurrentFile = state.currentFile
		ctx.Model = state.model
		ctx.Provider = state.provider
		ctx.TokensInput = state.tokensInput
		ctx.TokensOutput = state.tokensOutput
		ctx.Todos = state.todos
		ctx.State, ctx.QuestionText = InferSessionState(SessionSignals{
			LastRole:          state.lastRole,
			LastAssistantText: state.lastText,
			CurrentTool:       state.currentTool,
			PendingToolUse:    state.pendingToolUse,
		})
	}
	return ctx
}

func (r *CodexContextReader) resolveRolloutPath(path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		return ""
	}
	candidates := []string{
		filepath.Join(r.home, ".codex", path),
		filepath.Join(r.home, ".codex", "sessions", path),
		filepath.Join(r.home, ".codex", "archived_sessions", path),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

type codexRolloutState struct {
	task           string
	lastRole       string
	lastText       string
	pendingToolUse bool
	currentTool    string
	currentFile    string
	model          string
	provider       string
	tokensInput    int64
	tokensOutput   int64
	todos          []TodoItem
}

func readCodexRolloutState(path string) codexRolloutState {
	file, err := os.Open(path)
	if err != nil {
		return codexRolloutState{}
	}
	defer file.Close()

	state := codexRolloutState{}
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		updateCodexStateFromLine(line, &state)
	}
	return state
}

func updateCodexStateFromLine(line string, state *codexRolloutState) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}

	if msgs, ok := raw["input_messages"].([]any); ok {
		parts := make([]string, 0, len(msgs))
		for _, msg := range msgs {
			if s, ok := msg.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, strings.TrimSpace(s))
			}
		}
	if len(parts) > 0 {
		state.task = strings.Join(parts, " ")
		state.lastRole = "user"
		state.pendingToolUse = false
	}
	}

	if t, _ := raw["type"].(string); t == "response_item" || t == "event_msg" {
		state.lastRole = "assistant"
		if s := extractStringField(raw, "content", "text", "message"); s != "" {
			state.task = s
			state.lastText = s
		}
		if payload, ok := raw["payload"].(map[string]any); ok {
			if s := extractStringField(payload, "content", "text", "message"); s != "" {
				state.task = s
				state.lastText = s
			}
			if usage, ok := payload["usage"].(map[string]any); ok {
				state.tokensInput = int64(coerceFloat(usage["input_tokens"]))
				state.tokensOutput = int64(coerceFloat(usage["output_tokens"]))
			}
			if model, _ := payload["model"].(string); model != "" {
				state.model = model
			}
		}
	}

	// Codex rollout may contain tool_call blocks.
	if toolCalls, ok := raw["tool_calls"].([]any); ok && len(toolCalls) > 0 {
		state.pendingToolUse = true
		for _, tc := range toolCalls {
			block, _ := tc.(map[string]any)
			if block == nil {
				continue
			}
			if name, _ := block["name"].(string); name != "" {
				state.currentTool = name
			}
			if args, ok := block["arguments"].(map[string]any); ok {
				if fp, _ := args["file_path"].(string); fp != "" {
					state.currentFile = fp
				}
			}
		}
	}

	if t, _ := raw["type"].(string); t == "tool_result" || t == "function_call_output" {
		state.pendingToolUse = false
	}
}
