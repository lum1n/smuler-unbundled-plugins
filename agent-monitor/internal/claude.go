package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxInitialTranscriptRead bounds the first parse of a very large transcript
// to its tail; later refreshes only read newly appended bytes.
const maxInitialTranscriptRead = 16 * 1024 * 1024

// transcriptCacheTTL evicts cached transcript state that has not been used.
const transcriptCacheTTL = 10 * time.Minute

// ClaudeContextReader reads Claude Code session metadata and transcripts.
type ClaudeContextReader struct {
	baseDir string

	mu          sync.Mutex
	transcripts map[string]string // sessionID -> transcript path
	states      map[string]*claudeTranscriptCache
}

type claudeTranscriptCache struct {
	tail     jsonlTail
	state    claudeTranscriptState
	lastUsed time.Time
}

type claudeSessionMeta struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// NewClaudeContextReader creates a reader for Claude Code's local files.
func NewClaudeContextReader() *ClaudeContextReader {
	home, _ := os.UserHomeDir()
	return NewClaudeContextReaderAt(filepath.Join(home, ".claude"))
}

// NewClaudeContextReaderAt creates a reader rooted at an explicit ~/.claude dir.
func NewClaudeContextReaderAt(baseDir string) *ClaudeContextReader {
	return &ClaudeContextReader{
		baseDir:     baseDir,
		transcripts: make(map[string]string),
		states:      make(map[string]*claudeTranscriptCache),
	}
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

	transcript := r.findTranscript(meta.SessionID, meta.CWD)
	if transcript == "" {
		return ctx
	}

	state := r.transcriptState(transcript)
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
	// Claude Code names session files after the process id; try that first
	// before scanning the whole directory.
	if data, err := os.ReadFile(filepath.Join(sessionsDir, strconv.Itoa(pid)+".json")); err == nil {
		var meta claudeSessionMeta
		if json.Unmarshal(data, &meta) == nil && meta.PID == pid {
			return meta, true
		}
	}
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

// findTranscript locates <sessionID>.jsonl under ~/.claude/projects. It checks
// the directory derived from cwd first and otherwise only looks one level deep
// (projects/<dir>/<id>.jsonl) instead of walking the whole tree; hits are cached.
func (r *ClaudeContextReader) findTranscript(sessionID, cwd string) string {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	name := sessionID + ".jsonl"

	r.mu.Lock()
	cached := r.transcripts[sessionID]
	r.mu.Unlock()
	if cached != "" {
		if _, err := os.Stat(cached); err == nil {
			return cached
		}
	}

	projectsDir := filepath.Join(r.baseDir, "projects")
	found := ""
	if cwd != "" {
		candidate := filepath.Join(projectsDir, claudeProjectDirName(cwd), name)
		if _, err := os.Stat(candidate); err == nil {
			found = candidate
		}
	}
	if found == "" {
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			return ""
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			candidate := filepath.Join(projectsDir, e.Name(), name)
			if _, err := os.Stat(candidate); err == nil {
				found = candidate
				break
			}
		}
	}
	if found != "" {
		r.mu.Lock()
		r.transcripts[sessionID] = found
		r.mu.Unlock()
	}
	return found
}

// claudeProjectDirName mirrors Claude Code's project directory naming, which
// replaces every non-alphanumeric character of the cwd with '-'.
func claudeProjectDirName(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// transcriptState returns the parsed state for a transcript, reading only the
// bytes appended since the previous call.
func (r *ClaudeContextReader) transcriptState(path string) claudeTranscriptState {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	for p, c := range r.states {
		if now.Sub(c.lastUsed) > transcriptCacheTTL {
			delete(r.states, p)
		}
	}
	c := r.states[path]
	if c == nil {
		c = &claudeTranscriptCache{tail: jsonlTail{maxInitial: maxInitialTranscriptRead}}
		r.states[path] = c
	}
	c.lastUsed = now
	_ = c.tail.next(path,
		func() { c.state = claudeTranscriptState{} },
		func(line []byte) { updateClaudeStateFromLine(line, &c.state) })
	st := c.state
	st.todos = append([]TodoItem(nil), c.state.todos...)
	return st
}

type claudeTranscriptState struct {
	lastPrompt        string
	lastRole          string
	lastAssistantText string
	currentTool       string
	currentFile       string
	pendingToolUse    bool
	model             string
	provider          string
	tokensInput       int64
	tokensOutput      int64
	cacheRead         int64
	cacheWrite        int64
	todos             []TodoItem
}

func updateClaudeStateFromLine(line []byte, state *claudeTranscriptState) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
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
		// isMeta lines are synthetic (command caveats etc.), not user prompts.
		if isMeta, _ := raw["isMeta"].(bool); !isMeta {
			if prompt := extractClaudePrompt(msg); prompt != "" {
				state.lastPrompt = prompt
			}
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
					// TodoWrite carries the full todo list in its input.
					if name, _ := block["name"].(string); name == "TodoWrite" {
						if todosArr, ok := input["todos"].([]any); ok {
							state.todos = parseClaudeTodos(todosArr)
						}
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
	// Transcript lines unwrap to {"role":"user","content":...}. Tool results
	// also use the user role but carry no text blocks, so they yield "".
	if role, _ := raw["role"].(string); role == "user" {
		return extractStringField(raw, "content", "text")
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
