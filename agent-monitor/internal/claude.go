package internal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ClaudeContextReader reads Claude Code session metadata and transcripts.
type ClaudeContextReader struct {
	baseDir string
}

type claudeSessionMeta struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// NewClaudeContextReader creates a reader for Claude Code's local files.
func NewClaudeContextReader() *ClaudeContextReader {
	home, _ := os.UserHomeDir()
	return &ClaudeContextReader{baseDir: filepath.Join(home, ".claude")}
}

// ContextForPID returns Claude Code session context for the given process.
func (r *ClaudeContextReader) ContextForPID(pid int) AgentSession {
	if r == nil || pid <= 0 {
		return AgentSession{}
	}
	meta, ok := r.sessionMetaForPID(pid)
	if !ok {
		return AgentSession{}
	}

	ctx := AgentSession{
		AgentID:     "claude",
		DisplayName: "Claude Code",
		CWD:         meta.CWD,
		SessionID:   meta.SessionID,
		Available:   true,
	}
	if meta.CWD != "" {
		ctx.Task = filepath.Base(meta.CWD)
	}
	if meta.SessionID == "" {
		return ctx
	}

	transcript := r.findTranscript(meta.SessionID)
	if transcript == "" {
		return ctx
	}

	state := readClaudeTranscriptState(transcript)
	ctx.Task = state.lastPrompt
	ctx.CurrentTool = state.currentTool
	ctx.CurrentFile = state.currentFile
	ctx.Model = state.model
	ctx.Provider = state.provider
	ctx.TokensInput = state.tokensInput
	ctx.TokensOutput = state.tokensOutput
	ctx.CacheRead = state.cacheRead
	ctx.CacheWrite = state.cacheWrite
	ctx.Todos = state.todos
	ctx.State, ctx.QuestionText = InferSessionState(SessionSignals{
		LastRole:          state.lastRole,
		LastAssistantText: state.lastAssistantText,
		CurrentTool:       state.currentTool,
		PendingToolUse:    state.pendingToolUse,
	})
	return ctx
}

func (r *ClaudeContextReader) sessionMetaForPID(pid int) (claudeSessionMeta, bool) {
	sessionsDir := filepath.Join(r.baseDir, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return claudeSessionMeta{}, false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(sessionsDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var meta claudeSessionMeta
		if json.Unmarshal(data, &meta) == nil && meta.PID == pid {
			return meta, true
		}
	}
	return claudeSessionMeta{}, false
}

func (r *ClaudeContextReader) findTranscript(sessionID string) string {
	projectsDir := filepath.Join(r.baseDir, "projects")
	var found string
	_ = filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) == sessionID+".jsonl" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

type claudeTranscriptState struct {
	lastPrompt       string
	lastRole         string
	lastAssistantText string
	currentTool      string
	currentFile      string
	pendingToolUse   bool
	model            string
	provider         string
	tokensInput      int64
	tokensOutput     int64
	cacheRead        int64
	cacheWrite       int64
	todos            []TodoItem
}

func readClaudeTranscriptState(path string) claudeTranscriptState {
	file, err := os.Open(path)
	if err != nil {
		return claudeTranscriptState{}
	}
	defer file.Close()

	state := claudeTranscriptState{}
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		updateClaudeStateFromLine(line, &state)
	}
	return state
}

func updateClaudeStateFromLine(line string, state *claudeTranscriptState) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return
	}

	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		// Try top-level format used by queue-operation and some entries.
		msg = raw
	}

	role, _ := msg["role"].(string)
	msgType, _ := msg["type"].(string)

	// User prompt.
	if role == "user" || msgType == "user" {
		state.lastRole = "user"
		state.pendingToolUse = false
		if prompt := extractClaudePrompt(msg); prompt != "" {
			state.lastPrompt = prompt
		}
	}

	// Assistant message: model, usage, tool calls.
	if role == "assistant" || msgType == "assistant" || msgType == "message" {
		state.lastRole = "assistant"
		state.pendingToolUse = false
		if model, _ := msg["model"].(string); model != "" {
			state.model = model
		}
		if provider, _ := msg["provider"].(string); provider != "" {
			state.provider = provider
		}
		if usage, ok := msg["usage"].(map[string]any); ok {
			state.tokensInput = int64(coerceFloat(usage["input_tokens"]))
			state.tokensOutput = int64(coerceFloat(usage["output_tokens"]))
			state.cacheRead = int64(coerceFloat(usage["cache_read_input_tokens"]))
			state.cacheWrite = int64(coerceFloat(usage["cache_creation_input_tokens"]))
		}

		content, _ := msg["content"].([]any)
		for _, c := range content {
			block, _ := c.(map[string]any)
			if block == nil {
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType == "tool_use" || blockType == "toolCall" {
				if name, _ := block["name"].(string); name != "" {
					state.currentTool = name
				}
				state.pendingToolUse = true
				if input, ok := block["input"].(map[string]any); ok {
					if fp, _ := input["file_path"].(string); fp != "" {
						state.currentFile = fp
					}
				}
			}
			if blockType == "text" {
				if text, _ := block["text"].(string); text != "" {
					state.lastAssistantText = strings.TrimSpace(text)
				}
			}
		}
	}

	// Tool result: reflects the last executed tool.
	if role == "toolResult" || msgType == "tool_result" {
		state.pendingToolUse = false
		if name, _ := msg["toolName"].(string); name != "" {
			state.currentTool = name
		}
	}

	// TodoWrite tool results update todos.
	if state.currentTool == "TodoWrite" {
		if input, ok := msg["input"].(map[string]any); ok {
			if todosArr, ok := input["todos"].([]any); ok {
				state.todos = parseClaudeTodos(todosArr)
			}
		}
	}
}

func extractClaudePrompt(raw map[string]any) string {
	if prompt, ok := raw["user_prompt"].(string); ok {
		return strings.TrimSpace(prompt)
	}
	if t, _ := raw["type"].(string); t == "user" || t == "user-message" {
		if s := extractStringField(raw, "text", "content", "message"); s != "" {
			return s
		}
	}
	if msg, ok := raw["message"].(map[string]any); ok {
		if role, _ := msg["role"].(string); role == "user" {
			if s := extractStringField(msg, "content", "text"); s != "" {
				return s
			}
		}
	}
	return ""
}

func parseClaudeTodos(arr []any) []TodoItem {
	var todos []TodoItem
	for _, item := range arr {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		content, _ := m["content"].(string)
		status, _ := m["status"].(string)
		priority, _ := m["priority"].(string)
		if content == "" {
			continue
		}
		todos = append(todos, TodoItem{Content: content, Status: status, Priority: priority})
	}
	return todos
}

func coerceFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func extractStringField(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := raw[key].(string); ok {
			return strings.TrimSpace(s)
		}
		if arr, ok := raw[key].([]any); ok {
			parts := make([]string, 0, len(arr))
			for _, item := range arr {
				switch v := item.(type) {
				case string:
					parts = append(parts, strings.TrimSpace(v))
				case map[string]any:
					if text, ok := v["text"].(string); ok {
						parts = append(parts, strings.TrimSpace(text))
					}
				}
			}
			joined := strings.TrimSpace(strings.Join(parts, " "))
			if joined != "" {
				return joined
			}
		}
	}
	return ""
}
