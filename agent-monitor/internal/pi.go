package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PiContextReader reads Pi session files from ~/.pi/agent/sessions.
type PiContextReader struct {
	baseDir string

	mu     sync.Mutex
	states map[string]*piSessionCache
}

// piSessionCache keeps the parsed session tree so refreshes only parse newly
// appended entries and only re-derive context when something changed.
type piSessionCache struct {
	tail     jsonlTail
	state    piSessionState
	version  int
	derived  int
	result   AgentSession
	lastUsed time.Time
}

// NewPiContextReader creates a reader for Pi's local session files.
func NewPiContextReader() *PiContextReader {
	home, _ := os.UserHomeDir()
	return NewPiContextReaderAt(filepath.Join(home, ".pi", "agent", "sessions"))
}

// NewPiContextReaderAt creates a reader using an explicit base directory.
func NewPiContextReaderAt(baseDir string) *PiContextReader {
	return &PiContextReader{baseDir: baseDir, states: make(map[string]*piSessionCache)}
}

// ContextForPID returns Pi session context for the given process.
func (r *PiContextReader) ContextForPID(pid int) AgentSession {
	if r == nil || pid <= 0 {
		return AgentSession{}
	}
	cwd := cwdForPID(pid)
	if cwd == "" {
		return AgentSession{}
	}

	sessionFile := r.findSessionFile(cwd)
	if sessionFile == "" {
		return AgentSession{}
	}

	return r.readSession(sessionFile, cwd)
}

func (r *PiContextReader) findSessionFile(cwd string) string {
	if _, err := os.Stat(r.baseDir); err != nil {
		return ""
	}

	escaped := escapePiCWD(cwd)
	sessionsDir := filepath.Join(r.baseDir, "--"+escaped+"--")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return ""
	}

	var latest os.FileInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if latest == nil || info.ModTime().After(latest.ModTime()) {
			latest = info
		}
	}
	if latest == nil {
		return ""
	}
	return filepath.Join(sessionsDir, latest.Name())
}

func (r *PiContextReader) readSession(path, cwd string) AgentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.states == nil {
		r.states = make(map[string]*piSessionCache)
	}
	now := time.Now()
	for p, c := range r.states {
		if now.Sub(c.lastUsed) > transcriptCacheTTL {
			delete(r.states, p)
		}
	}
	c := r.states[path]
	if c == nil {
		c = &piSessionCache{state: piSessionState{cwd: cwd}, derived: -1}
		r.states[path] = c
	}
	c.lastUsed = now

	err := c.tail.next(path,
		func() {
			c.state = piSessionState{cwd: cwd}
			c.version++
		},
		func(line []byte) {
			var entry piEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				return
			}
			c.state.addEntry(entry)
			c.version++
		})
	if err != nil {
		delete(r.states, path)
		return AgentSession{}
	}
	if c.derived != c.version {
		c.result = c.state.toAgentSession()
		c.derived = c.version
	}
	return c.result
}

type piEntry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	Name      string          `json:"name"`
	Provider  string          `json:"provider"`
	ModelID   string          `json:"modelId"`
	CWD       string          `json:"cwd"`
}

type piSessionState struct {
	cwd          string
	sessionID    string
	sessionName  string
	entries      map[string]piEntry
	children     map[string][]string
	rootID       string
	leafID       string
	lastPrompt   string
	currentTool  string
	currentFile  string
	model        string
	provider     string
	tokensInput  int64
	tokensOutput int64
	cacheRead    int64
	cacheWrite   int64
	cost         float64
}

func (s *piSessionState) addEntry(e piEntry) {
	switch e.Type {
	case "session":
		if e.CWD != "" {
			s.cwd = e.CWD
		}
		s.sessionID = e.ID
		return
	case "session_info":
		if e.Name != "" {
			s.sessionName = e.Name
		}
		return
	}

	if e.ID == "" {
		return
	}
	if s.entries == nil {
		s.entries = make(map[string]piEntry)
		s.children = make(map[string][]string)
	}
	s.entries[e.ID] = e

	if e.ParentID == "" {
		s.rootID = e.ID
	} else {
		s.children[e.ParentID] = append(s.children[e.ParentID], e.ID)
	}

	// Track the chronologically latest entry as the approximate leaf.
	if s.leafID == "" || e.Timestamp > s.entries[s.leafID].Timestamp {
		s.leafID = e.ID
	}
}

func (s *piSessionState) toAgentSession() AgentSession {
	if s.leafID == "" {
		return AgentSession{}
	}

	// Derived fields are recomputed from the tree on every call.
	s.lastPrompt, s.currentTool, s.currentFile = "", "", ""
	s.model, s.provider = "", ""
	s.tokensInput, s.tokensOutput, s.cacheRead, s.cacheWrite = 0, 0, 0, 0
	s.cost = 0
	s.walkLeafToRoot(s.leafID)

	task := s.sessionName
	if task == "" {
		task = s.lastPrompt
	}

	state, questionText := s.inferState()

	return AgentSession{
		AgentID:      "pi",
		DisplayName:  "Pi",
		SessionID:    s.sessionID,
		Task:         task,
		CWD:          s.cwd,
		CurrentTool:  s.currentTool,
		CurrentFile:  s.currentFile,
		Model:        s.model,
		Provider:     s.provider,
		Cost:         s.cost,
		TokensInput:  s.tokensInput,
		TokensOutput: s.tokensOutput,
		CacheRead:    s.cacheRead,
		CacheWrite:   s.cacheWrite,
		State:        state,
		QuestionText: questionText,
		Available:    true,
	}
}

func (s *piSessionState) inferState() (string, string) {
	leaf, ok := s.entries[s.leafID]
	if !ok {
		return "running", ""
	}

	activeTool := leaf.Type == "toolCall" || leaf.Type == "bashExecution"
	pendingTool := false
	lastRole := ""
	lastText := ""

	switch leaf.Type {
	case "toolCall", "bashExecution":
		return InferSessionState(SessionSignals{ActiveToolExec: true, CurrentTool: s.currentTool})
	case "message":
		var msg map[string]any
		if err := json.Unmarshal(leaf.Message, &msg); err != nil {
			return "running", ""
		}
		lastRole, _ = msg["role"].(string)
		if lastRole == "assistant" {
			lastText = extractPiAssistantText(msg)
			for _, c := range toAnySlice(msg["content"]) {
				block, _ := c.(map[string]any)
				if block == nil {
					continue
				}
				if t, _ := block["type"].(string); t == "toolCall" {
					pendingTool = true
				}
			}
		}
	}

	return InferSessionState(SessionSignals{
		LastRole:          lastRole,
		LastAssistantText: lastText,
		CurrentTool:       s.currentTool,
		PendingToolUse:    pendingTool,
		ActiveToolExec:    activeTool,
	})
}

func toAnySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

func extractPiAssistantText(msg map[string]any) string {
	content := msg["content"]
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, c := range v {
			block, _ := c.(map[string]any)
			if block == nil {
				continue
			}
			if t, _ := block["type"].(string); t == "text" {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return ""
}

// walkLeafToRoot collects the active branch and applies it root-first, so the
// latest prompt/tool/model win (applying leaf-first left the oldest values).
func (s *piSessionState) walkLeafToRoot(leafID string) {
	visited := make(map[string]bool)
	var path []piEntry
	current := leafID
	for current != "" && !visited[current] {
		visited[current] = true
		e, ok := s.entries[current]
		if !ok {
			break
		}
		path = append(path, e)
		current = e.ParentID
	}
	for i := len(path) - 1; i >= 0; i-- {
		s.applyEntry(path[i])
	}
}

func (s *piSessionState) applyEntry(e piEntry) {
	if e.Type != "message" || len(e.Message) == 0 {
		if e.Type == "model_change" {
			if e.Provider != "" {
				s.provider = e.Provider
			}
			if e.ModelID != "" {
				s.model = e.ModelID
			}
		}
		return
	}

	var msg map[string]any
	if err := json.Unmarshal(e.Message, &msg); err != nil {
		return
	}

	role, _ := msg["role"].(string)

	// User prompt.
	if role == "user" {
		if prompt := extractPiUserPrompt(msg); prompt != "" {
			s.lastPrompt = prompt
		}
		return
	}

	// Assistant message: model, provider, usage, tool calls.
	if role == "assistant" {
		if model, _ := msg["model"].(string); model != "" {
			s.model = model
		}
		if provider, _ := msg["provider"].(string); provider != "" {
			s.provider = provider
		}
		if usage, ok := msg["usage"].(map[string]any); ok {
			s.tokensInput += int64(coerceFloat(usage["input"]))
			s.tokensOutput += int64(coerceFloat(usage["output"]))
			s.cacheRead += int64(coerceFloat(usage["cacheRead"]))
			s.cacheWrite += int64(coerceFloat(usage["cacheWrite"]))
			if cost, ok := usage["cost"].(map[string]any); ok {
				s.cost += coerceFloat(cost["total"])
			}
		}

		content, _ := msg["content"].([]any)
		for _, c := range content {
			block, _ := c.(map[string]any)
			if block == nil {
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType == "toolCall" {
				if name, _ := block["name"].(string); name != "" {
					s.currentTool = name
				}
				if args, ok := block["arguments"].(map[string]any); ok {
					if fp := piFilePathFromArgs(args); fp != "" {
						s.currentFile = fp
					}
				}
			}
		}
		return
	}

	// Tool result: reflects the last executed tool.
	if role == "toolResult" {
		if name, _ := msg["toolName"].(string); name != "" {
			s.currentTool = name
		}
		return
	}

	// Bash execution.
	if role == "bashExecution" {
		s.currentTool = "bash"
		return
	}
}

func extractPiUserPrompt(msg map[string]any) string {
	content := msg["content"]
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, c := range v {
			block, _ := c.(map[string]any)
			if block == nil {
				continue
			}
			if t, _ := block["type"].(string); t == "text" {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	return ""
}

func piFilePathFromArgs(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "file", "filepath"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func escapePiCWD(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// findPiLeaf finds the entry with the most children (a heuristic for the
// current branch tip). Unused in favor of the latest timestamp leaf.
func (s *piSessionState) findPiLeaf() string {
	if s.rootID == "" {
		return ""
	}
	type queueItem struct {
		id    string
		depth int
	}
	leaf := s.rootID
	maxDepth := 0
	q := []queueItem{{s.rootID, 0}}
	visited := make(map[string]bool)
	for len(q) > 0 {
		cur := q[0]
		q = q[1:]
		if visited[cur.id] {
			continue
		}
		visited[cur.id] = true
		if cur.depth > maxDepth {
			maxDepth = cur.depth
			leaf = cur.id
		}
		children := s.children[cur.id]
		sort.Strings(children)
		for _, child := range children {
			q = append(q, queueItem{child, cur.depth + 1})
		}
	}
	return leaf
}
