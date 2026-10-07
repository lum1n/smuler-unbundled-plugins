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
	// A tool result is the last entry: the model is about to continue. An
	// assistant reply after the tool means the turn ended, so a remembered
	// CurrentTool alone must not keep the agent "working" forever.
	if s.CurrentTool != "" && s.LastRole != "user" && s.LastRole != "assistant" && s.LastRole != "" {
		return "working", ""
	}
	switch s.LastRole {
	case "user":
		return "working", ""
	default:
		// Assistant spoke with no pending tools: the turn is over and the
		// agent waits for input. Reporting "thinking" here counted idle
		// sessions as active and overrode hook-reported completion.
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
	// A hook-reported terminal state stands until there is evidence of new
	// activity; an idle transcript must not hide "completed"/"error".
	if existing == "completed" || existing == "error" {
		switch incoming {
		case "working", "question":
			return incoming
		}
		return existing
	}
	if statePriority(existing) >= statePriority(incoming) {
		return existing
	}
	return incoming
}

// OverlayWatcherState applies reader/hook state onto a watcher-owned session.
// Watcher state wins unless the watcher row is idle/unbound and incoming is question.
func OverlayWatcherState(watcherState, incoming string) string {
	if watcherState == "" {
		return incoming
	}
	if incoming == "" {
		return watcherState
	}
	if incoming == "question" && (watcherState == "idle" || watcherState == "running" || watcherState == "unbound") {
		return incoming
	}
	return watcherState
}
