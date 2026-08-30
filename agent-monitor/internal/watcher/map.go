package watcher

// MapState translates an agent-watcher state into the smuler agent-domain string.
// Unbound windows are idle-ish and must not emit alerts.
func MapState(watcherState string, unbound bool) string {
	if unbound {
		return "idle"
	}
	switch watcherState {
	case "waiting-permission":
		return "question"
	case "running-tool":
		return "working"
	case "thinking":
		return "thinking"
	case "idle", "":
		return "idle"
	case "errored":
		return "error"
	default:
		return watcherState
	}
}

// DisplayName returns a human label for a watcher kind / agentId.
func DisplayName(kind string) string {
	switch kind {
	case "claude":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	case "pi":
		return "Pi"
	case "cursor":
		return "Cursor"
	case "aider":
		return "Aider"
	case "archer":
		return "Archer"
	default:
		if kind == "" {
			return "Agent"
		}
		return kind
	}
}

// EmitsEvent reports whether a mapped state should fire a host notification.
func EmitsEvent(mapped string) string {
	switch mapped {
	case "question":
		return "agent_question"
	case "error":
		return "agent_error"
	case "completed":
		return "agent_completed"
	default:
		return ""
	}
}
