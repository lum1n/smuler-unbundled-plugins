package internal

import "time"

// TodoItem tracks a single task inside an agent session.
type TodoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// AgentSession is the canonical representation of a running or recently
// finished agent. It is produced by combining process detection, shell/native
// hook events, and agent-specific context readers.
type AgentSession struct {
	ID           string `json:"id"`
	AgentID      string `json:"agentId"`      // claude, codex, opencode, pi, aider, archer, ...
	DisplayName  string `json:"displayName"`  // "Claude Code", "Codex", "Pi", ...
	PID          int    `json:"pid"`
	Command      string `json:"command"`      // raw command line
	Task         string `json:"task"`         // current task / last user prompt
	State        string `json:"state"`        // running | working | thinking | question | completed | error | paused
	OutputTail   string `json:"outputTail"`
	SessionID    string `json:"sessionId"`    // agent-native session id when known
	CurrentFile  string `json:"currentFile"`  // file currently being read/written
	CurrentTool  string `json:"currentTool"`  // current tool name
	Model        string `json:"model"`
	Provider     string `json:"provider"`
	Cost         float64 `json:"cost"`
	TokensInput  int64   `json:"tokensInput"`
	TokensOutput int64   `json:"tokensOutput"`
	CacheRead    int64   `json:"cacheRead"`
	CacheWrite   int64   `json:"cacheWrite"`
	FilesChanged int     `json:"filesChanged"`
	Additions    int     `json:"additions"`
	Deletions    int     `json:"deletions"`
	CWD          string `json:"cwd"`
	RepoRoot     string `json:"repoRoot"`
	RepoName     string `json:"repoName"`
	RepoFullName string `json:"repoFullName"`
	Branch       string `json:"branch"`
	IsRepoDirty  bool   `json:"isRepoDirty"`
	QuestionText string `json:"questionText,omitempty"`
	Todos        []TodoItem `json:"todos"`
	StartTime    int64  `json:"startTime"`    // ms since epoch
	UpdatedAt    int64  `json:"updatedAt"`    // ms since epoch
	ExitCode     *int   `json:"exitCode,omitempty"`
	Available    bool   `json:"available"`    // true when context reader found data
}

// IsTerminalState returns true when the agent has finished.
func (a AgentSession) IsTerminalState() bool {
	return a.State == "completed" || a.State == "error"
}

// Duration returns the time elapsed since StartTime.
func (a AgentSession) Duration() time.Duration {
	start := time.UnixMilli(a.StartTime)
	if start.IsZero() || a.StartTime == 0 {
		return 0
	}
	return time.Since(start)
}

// Clone returns a deep-ish copy of the session.
func (a AgentSession) Clone() AgentSession {
	cp := a
	if a.ExitCode != nil {
		code := *a.ExitCode
		cp.ExitCode = &code
	}
	cp.Todos = make([]TodoItem, len(a.Todos))
	copy(cp.Todos, a.Todos)
	return cp
}
