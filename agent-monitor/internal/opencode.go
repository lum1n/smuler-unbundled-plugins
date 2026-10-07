package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenCodeContextReader reads session context from the OpenCode SQLite database.
type OpenCodeContextReader struct {
	dbPath string
}

type openCodeSessionRow struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Agent        string  `json:"agent"`
	ModelID      string  `json:"model_id"`
	Cost         float64 `json:"cost"`
	TokensInput  int64   `json:"tokens_input"`
	TokensOutput int64   `json:"tokens_output"`
	FilesChanged int     `json:"summary_files"`
	Additions    int     `json:"summary_additions"`
	Deletions    int     `json:"summary_deletions"`
}

type openCodePartRow struct {
	Data string `json:"data"`
}

// NewOpenCodeContextReader creates a reader for OpenCode's local database.
func NewOpenCodeContextReader() *OpenCodeContextReader {
	home, _ := os.UserHomeDir()
	return &OpenCodeContextReader{
		dbPath: filepath.Join(home, ".local", "share", "opencode", "opencode.db"),
	}
}

// ContextForPID returns OpenCode session context for the given process.
func (r *OpenCodeContextReader) ContextForPID(pid int) AgentSession {
	if r == nil || pid <= 0 {
		return AgentSession{}
	}
	if _, err := os.Stat(r.dbPath); err != nil {
		return AgentSession{}
	}

	cwd := cwdForPID(pid)
	if cwd == "" {
		return AgentSession{}
	}

	sessions, err := sqliteQueryJSON[openCodeSessionRow](
		r.dbPath,
		fmt.Sprintf(
			"SELECT id, title, agent, json_extract(model, '$.id') AS model_id, cost, tokens_input, tokens_output, summary_files, summary_additions, summary_deletions FROM session WHERE directory=%s ORDER BY time_updated DESC LIMIT 1",
			sqliteQuote(cwd),
		),
	)
	if err != nil || len(sessions) == 0 {
		return AgentSession{}
	}

	session := sessions[0]
	ctx := AgentSession{
		AgentID:      session.Agent,
		DisplayName:  "OpenCode",
		SessionID:    session.ID,
		Task:         session.Title,
		Model:        session.ModelID,
		Cost:         session.Cost,
		TokensInput:  session.TokensInput,
		TokensOutput: session.TokensOutput,
		FilesChanged: session.FilesChanged,
		Additions:    session.Additions,
		Deletions:    session.Deletions,
		CWD:          cwd,
		Available:    true,
	}

	parts, err := sqliteQueryJSON[openCodePartRow](
		r.dbPath,
		fmt.Sprintf(
			"SELECT p.data FROM part p JOIN message m ON p.message_id = m.id WHERE p.session_id=%s AND json_extract(m.data, '$.role')='user' AND json_extract(p.data, '$.type')='text' ORDER BY p.time_created DESC LIMIT 1",
			sqliteQuote(session.ID),
		),
	)
	if err == nil && len(parts) > 0 {
		ctx.Task = extractPartText(parts[0].Data)
	}

	todos, err := sqliteQueryJSON[TodoItem](
		r.dbPath,
		fmt.Sprintf(
			"SELECT content, status, priority FROM todo WHERE session_id=%s ORDER BY position",
			sqliteQuote(session.ID),
		),
	)
	if err == nil {
		ctx.Todos = todos
	}

	ctx.State, ctx.QuestionText = r.inferState(session.ID)

	return ctx
}

type openCodePartStateRow struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

func (r *OpenCodeContextReader) inferState(sessionID string) (string, string) {
	messages, err := sqliteQueryJSON[openCodeMessageRow](
		r.dbPath,
		fmt.Sprintf(
			"SELECT data FROM message WHERE session_id=%s ORDER BY time_created DESC LIMIT 3",
			sqliteQuote(sessionID),
		),
	)
	if err != nil || len(messages) == 0 {
		return "running", ""
	}

	last := messages[0]
	lastRole := extractMessageField(last.Data, "role")
	lastContent := extractMessageField(last.Data, "content")

	parts, err := sqliteQueryJSON[openCodePartStateRow](
		r.dbPath,
		fmt.Sprintf(
			"SELECT json_extract(p.data, '$.type') AS type, p.data AS data FROM part p JOIN message m ON p.message_id = m.id WHERE p.session_id=%s ORDER BY p.time_created DESC LIMIT 5",
			sqliteQuote(sessionID),
		),
	)
	pendingTool := false
	currentTool := ""
	if err == nil {
		for _, part := range parts {
			partType := strings.ToLower(part.Type)
			if partType == "tool" || partType == "tool-call" || partType == "tool_call" || partType == "tool-invocation" {
				pendingTool = true
				if name := extractPartToolName(part.Data); name != "" {
					currentTool = name
				}
				break
			}
		}
	}

	return InferSessionState(SessionSignals{
		LastRole:          lastRole,
		LastAssistantText: lastContent,
		CurrentTool:       currentTool,
		PendingToolUse:    pendingTool,
	})
}

func extractPartToolName(data string) string {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return ""
	}
	for _, key := range []string{"tool", "toolName", "name", "tool_name"} {
		if v, ok := raw[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

type openCodeMessageRow struct {
	Role string `json:"role"`
	Data string `json:"data"`
}

func extractMessageField(data, field string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		return ""
	}
	if v, ok := m[field].(string); ok {
		return v
	}
	return ""
}

func looksLikeQuestion(text string) bool {
	// Only the end of the reply matters: a "?" in code, URLs or an earlier
	// paragraph of a long final summary is not a question to the user.
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	lines := strings.Split(text, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	last = strings.TrimRight(last, "*_` ")
	if strings.HasSuffix(last, "?") {
		return true
	}
	lower := strings.ToLower(last)
	phrases := []string{
		"do you want", "should i", "would you like", "can you confirm",
		"please confirm", "confirm to", "awaiting confirmation", "needs your approval",
		"permission to", "allow me to", "shall i",
	}
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func sqliteQueryJSON[T any](dbPath, query string) ([]T, error) {
	out, err := commandOutput("sqlite3", "-json", dbPath, query)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var rows []T
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func extractPartText(data string) string {
	var p struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Text)
}

func sqliteQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
