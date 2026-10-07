package internal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CodexContextReader reads Codex thread metadata and rollout logs.
type CodexContextReader struct {
	home string
	db   string

	mu     sync.Mutex
	states map[string]*codexRolloutCache
}

type codexRolloutCache struct {
	tail     jsonlTail
	state    codexRolloutState
	lastUsed time.Time
}

type codexThreadRow struct {
	ID        string `json:"id"`
	Rollout   string `json:"rollout_path"`
	CWD       string `json:"cwd"`
	Title     string `json:"title"`
	GitBranch string `json:"git_branch"`
	Archived  int    `json:"archived"`
	UpdatedAt int64  `json:"updated_at"`
}

// NewCodexContextReader creates a reader for Codex's local SQLite + rollout logs.
func NewCodexContextReader() *CodexContextReader {
	home, _ := os.UserHomeDir()
	return &CodexContextReader{
		home:   home,
		db:     filepath.Join(home, ".codex", "logs_2.sqlite"),
		states: make(map[string]*codexRolloutCache),
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
		state := r.rolloutState(rollout)
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

// rolloutState returns the parsed rollout state, reading only bytes appended
// since the previous call.
func (r *CodexContextReader) rolloutState(path string) codexRolloutState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states == nil {
		r.states = make(map[string]*codexRolloutCache)
	}
	now := time.Now()
	for p, c := range r.states {
		if now.Sub(c.lastUsed) > transcriptCacheTTL {
			delete(r.states, p)
		}
	}
	c := r.states[path]
	if c == nil {
		c = &codexRolloutCache{tail: jsonlTail{maxInitial: maxInitialTranscriptRead}}
		r.states[path] = c
	}
	c.lastUsed = now
	_ = c.tail.next(path,
		func() { c.state = codexRolloutState{} },
		func(line []byte) { updateCodexStateFromLine(line, &c.state) })
	st := c.state
	st.todos = append([]TodoItem(nil), c.state.todos...)
	return st
}

func updateCodexStateFromLine(line []byte, state *codexRolloutState) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
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

	lineType, _ := raw["type"].(string)
	payload, _ := raw["payload"].(map[string]any)
	payloadType := ""
	if payload != nil {
		payloadType, _ = payload["type"].(string)
	}

	switch lineType {
	case "turn_context":
		if model, _ := payload["model"].(string); model != "" {
			state.model = model
		}
	case "event_msg":
		switch payloadType {
		case "user_message":
			// Only real user prompts arrive as user_message events; user-role
			// response items also include injected environment context.
			if msg, _ := payload["message"].(string); strings.TrimSpace(msg) != "" {
				state.task = strings.TrimSpace(msg)
			}
			state.lastRole = "user"
			state.pendingToolUse = false
		case "agent_message":
			if msg, _ := payload["message"].(string); strings.TrimSpace(msg) != "" {
				state.lastText = strings.TrimSpace(msg)
				state.lastRole = "assistant"
			}
		case "token_count":
			if info, ok := payload["info"].(map[string]any); ok {
				if usage, ok := info["total_token_usage"].(map[string]any); ok {
					state.tokensInput = int64(coerceFloat(usage["input_tokens"]))
					state.tokensOutput = int64(coerceFloat(usage["output_tokens"]))
				}
			}
		case "task_complete", "turn_aborted":
			state.lastRole = "assistant"
			state.pendingToolUse = false
			state.currentTool = ""
			if msg, _ := payload["last_agent_message"].(string); strings.TrimSpace(msg) != "" {
				state.lastText = strings.TrimSpace(msg)
			}
		}
	case "response_item":
		switch payloadType {
		case "message":
			role, _ := payload["role"].(string)
			switch role {
			case "user":
				state.lastRole = "user"
				state.pendingToolUse = false
			case "assistant":
				state.lastRole = "assistant"
				state.pendingToolUse = false
				if s := extractStringField(payload, "content", "text"); s != "" {
					state.lastText = s
				}
			}
			if model, _ := payload["model"].(string); model != "" {
				state.model = model
			}
		case "function_call", "custom_tool_call", "local_shell_call":
			name, _ := payload["name"].(string)
			if name == "" && payloadType == "local_shell_call" {
				name = "shell"
			}
			if name != "" {
				state.currentTool = name
			}
			state.lastRole = "assistant"
			state.pendingToolUse = true
		case "function_call_output", "custom_tool_call_output":
			state.lastRole = "tool"
			state.pendingToolUse = false
		}
	}

	// Older rollout formats may contain top-level tool_call blocks.
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

	if lineType == "tool_result" || lineType == "function_call_output" {
		state.pendingToolUse = false
	}
}
