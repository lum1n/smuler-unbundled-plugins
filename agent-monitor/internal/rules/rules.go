package rules

import "time"

// RuleKind identifies the type of agent configuration file.
type RuleKind string

const (
	KindAgentsMd    RuleKind = "agents_md"
	KindCursorRule  RuleKind = "cursor_rule"
	KindClaudeSkill RuleKind = "claude_skill"
	KindClaudeMd    RuleKind = "claude_md"
	KindCodexConfig RuleKind = "codex_config"
	KindPiSystem    RuleKind = "pi_system"
	KindOpenCode    RuleKind = "opencode_config"
	KindAiderConfig RuleKind = "aider_config"
	KindUnknown     RuleKind = "unknown"
)

// RuleFile represents a single discovered agent configuration file.
type RuleFile struct {
	Path         string    `json:"path"`
	RepoRoot     string    `json:"repoRoot"`
	RepoName     string    `json:"repoName"`
	Kind         RuleKind  `json:"kind"`
	Title        string    `json:"title"`
	Excerpt      string    `json:"excerpt"`
	LastModified time.Time `json:"lastModified"`
	Size         int64     `json:"size"`
}

// String returns the raw string value of the rule kind.
func (k RuleKind) String() string { return string(k) }

// DisplayName returns a human-readable name for the rule kind.
func (k RuleKind) DisplayName() string {
	switch k {
	case KindAgentsMd:
		return "Project Rules"
	case KindCursorRule:
		return "Cursor Rule"
	case KindClaudeSkill:
		return "Claude Skill"
	case KindClaudeMd:
		return "Claude Settings"
	case KindCodexConfig:
		return "Codex Settings"
	case KindPiSystem:
		return "Pi System Prompt"
	case KindOpenCode:
		return "OpenCode Config"
	case KindAiderConfig:
		return "Aider Config"
	default:
		return "Agent Config"
	}
}

// IconHint returns a suggested SF Symbol name for the rule kind.
func (k RuleKind) IconHint() string {
	switch k {
	case KindAgentsMd:
		return "doc.text"
	case KindCursorRule:
		return "cursorarrow.rays"
	case KindClaudeSkill:
		return "brain.head.profile"
	case KindClaudeMd:
		return "gear"
	case KindCodexConfig:
		return "terminal"
	case KindPiSystem:
		return "mic"
	case KindOpenCode:
		return "network"
	case KindAiderConfig:
		return "bandage"
	default:
		return "doc"
	}
}
