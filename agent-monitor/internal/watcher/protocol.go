package watcher

import "strconv"

// ProtocolV is the agent-watcher NDJSON version.
const ProtocolV = 1

// Event is a v1 NDJSON line from agent-watcher.
type Event struct {
	V    int    `json:"v,omitempty"`
	Type string `json:"type"`

	Role string `json:"role,omitempty"`
	Pong bool   `json:"pong,omitempty"`
	Via  string `json:"via,omitempty"`

	Agents []AgentInfo `json:"agents,omitempty"`

	Session    string `json:"session,omitempty"`
	Window     int    `json:"window,omitempty"`
	Kind       string `json:"kind,omitempty"`
	State      string `json:"state,omitempty"`
	Path       string `json:"path,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	ToolTarget string `json:"toolTarget,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Attached   bool   `json:"attached,omitempty"`
	Windows    int    `json:"windows,omitempty"`
	AgeMs      int64  `json:"ageMs,omitempty"`
	Unbound    bool   `json:"unbound,omitempty"`
	Reason     string `json:"reason,omitempty"`

	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Ts      int64  `json:"ts,omitempty"`
}

// AgentInfo is one snapshot.agents[] entry.
type AgentInfo struct {
	Session    string `json:"session"`
	Window     int    `json:"window"`
	Kind       string `json:"kind"`
	Attached   bool   `json:"attached,omitempty"`
	Windows    int    `json:"windows,omitempty"`
	Path       string `json:"path,omitempty"`
	State      string `json:"state,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	ToolTarget string `json:"toolTarget,omitempty"`
	Summary    string `json:"summary,omitempty"`
	AgeMs      int64  `json:"ageMs,omitempty"`
	Unbound    bool   `json:"unbound,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
}

// Control is a stdin/socket command line.
type Control struct {
	Cmd       string `json:"cmd"`
	Session   string `json:"session,omitempty"`
	Window    int    `json:"window,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Path      string `json:"path,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

// SessionKey is the stable store id for a tmux agent window.
func SessionKey(session string, window int) string {
	return "tmux:" + session + ":w" + strconv.Itoa(window)
}
