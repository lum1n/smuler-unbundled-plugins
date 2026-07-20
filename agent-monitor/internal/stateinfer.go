package internal

// SessionSignals holds transcript-derived hints shared across agent context readers.
type SessionSignals struct {
	LastRole          string // user, assistant, tool
	LastAssistantText string
	CurrentTool       string
	PendingToolUse    bool // tool_use issued without a matching tool_result yet
	ActiveToolExec    bool // leaf entry is an in-flight tool execution (Pi toolCall/bashExecution)
}

// InferSessionState applies the same state rules for Claude, Codex, OpenCode, and Pi.
// Returns internal state names (running, working, thinking, question).
func InferSessionState(s SessionSignals) (state, questionText string) {
	if s.LastRole == "assistant" && looksLikeQuestion(s.LastAssistantText) {
		return "question", s.LastAssistantText
	}
	if s.ActiveToolExec || s.PendingToolUse {
		return "working", ""
	}
	if s.CurrentTool != "" && s.LastRole != "user" {
		return "working", ""
	}
	switch s.LastRole {
	case "user":
		return "working", ""
	case "assistant":
		// Assistant spoke with no pending tools — likely between turns; prefer
		// thinking over idle while the process is still alive.
		return "thinking", ""
	default:
		return "running", ""
	}
}

// statePriority ranks active states so fresher hook/store signals are not
// downgraded by context readers during enrichment.
func statePriority(state string) int {
	switch state {
	case "question":
		return 5
	case "working":
		return 4
	case "thinking":
		return 3
	case "paused":
		return 2
	case "running", "idle":
		return 1
	default:
		return 0
	}
}

// MergeAgentState picks the higher-priority state when combining store/hook
// data with context-reader enrichment.
func MergeAgentState(existing, incoming string) string {
	if existing == "" {
		return incoming
	}
	if incoming == "" {
		return existing
	}
	if statePriority(existing) >= statePriority(incoming) {
		return existing
	}
	return incoming
}
