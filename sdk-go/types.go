package sdk

import (
	"encoding/json"
)

// InitializeParams is sent by the host during the "initialize" handshake.
type InitializeParams struct {
	ProtocolVersion string                `json:"protocolVersion"`
	PluginID        string                `json:"pluginId"`
	Config          map[string]string     `json:"config"`
	Auth            *AuthContext          `json:"auth,omitempty"`
	ProviderAuths   []ProviderAuthContext `json:"providerAuths,omitempty"`
	PeerSnapshots   []Snapshot            `json:"peerSnapshots,omitempty"`
}

// AuthContext holds a legacy single-token auth.
type AuthContext struct {
	AccountID string `json:"accountId"`
}

// ProviderAuthContext holds a per-provider credential.
type ProviderAuthContext struct {
	ProviderID   string `json:"providerId"`
	Kind         string `json:"kind"`
	AccountID    string `json:"accountId,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
	CookieHeader string `json:"cookieHeader,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
}

// Snapshot is the plugin status payload returned for getStatus/refresh.
type Snapshot struct {
	PluginID     string  `json:"pluginId"`
	State        string  `json:"state"`
	Summary      Summary `json:"summary"`
	Items        []Item  `json:"items"`
	Actions      []Action `json:"actions"`
	Alerts       []Alert `json:"alerts"`
	RefreshAfter int     `json:"refreshAfter"`
	Health       string  `json:"health"`
}

// MarshalJSON ensures nil slices encode as [] instead of null.
func (s Snapshot) MarshalJSON() ([]byte, error) {
	type alias Snapshot
	a := alias(s)
	if a.Items == nil {
		a.Items = []Item{}
	}
	if a.Actions == nil {
		a.Actions = []Action{}
	}
	if a.Alerts == nil {
		a.Alerts = []Alert{}
	}
	return json.Marshal(a)
}

// Summary is the top-line status shown in the menu bar.
type Summary struct {
	Title    string `json:"title"`
	Value    string `json:"value"`
	Trend    string `json:"trend"`
	Severity string `json:"severity"`
	IconHint string `json:"iconHint"`
}

// Item is a single row in the plugin's detail view.
type Item struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Subtitle  string            `json:"subtitle,omitempty"`
	Detail    string            `json:"detail,omitempty"`
	Severity  string            `json:"severity"`
	Timestamp string            `json:"timestamp,omitempty"`
	DeepLink  string            `json:"deepLink,omitempty"`
	Actions   []Action          `json:"actions"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Action is a button / deep-link the user can activate.
type Action struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ActionResult is returned from performAction.
// Set Window to ask the host to open a themed secondary window with structured content.
// Set AI to ask the host to run a well-known local AI task and update that window.
type ActionResult struct {
	Success bool           `json:"success"`
	Data    string         `json:"data,omitempty"`
	Error   string         `json:"error,omitempty"`
	Window  *WindowContent `json:"window,omitempty"`
	AI      *AIAssist      `json:"ai,omitempty"`
}

// AIAssist asks the host to run a local AI task against Input and update WindowID.
// Supported v1 tasks: summarize_bullets, explain, risk_review, triage_next, draft_reply, extract_actions.
type AIAssist struct {
	Task         string `json:"task"`
	Input        string `json:"input"`
	WindowID     string `json:"windowId"`
	SectionTitle string `json:"sectionTitle,omitempty"`
}

// Well-known AI assist tasks (host-defined prompts).
const (
	AITaskSummarizeBullets = "summarize_bullets"
	AITaskExplain          = "explain"
	AITaskRiskReview       = "risk_review"
	AITaskTriageNext       = "triage_next"
	AITaskDraftReply       = "draft_reply"
	AITaskExtractActions   = "extract_actions"
)

// AITaskSectionTitle returns the default section title for a well-known task.
func AITaskSectionTitle(task string) string {
	switch task {
	case AITaskSummarizeBullets:
		return "Summary"
	case AITaskExplain:
		return "Explanation"
	case AITaskRiskReview:
		return "Risk Review"
	case AITaskTriageNext:
		return "Next Actions"
	case AITaskDraftReply:
		return "Draft"
	case AITaskExtractActions:
		return "Follow-ups"
	default:
		return "Result"
	}
}

// AITaskLoadingLabel returns the loading placeholder for a well-known task.
func AITaskLoadingLabel(task string) string {
	switch task {
	case AITaskSummarizeBullets:
		return "Generating summary..."
	case AITaskExplain:
		return "Explaining..."
	case AITaskRiskReview:
		return "Reviewing risks..."
	case AITaskTriageNext:
		return "Triaging..."
	case AITaskDraftReply:
		return "Drafting reply..."
	case AITaskExtractActions:
		return "Extracting actions..."
	default:
		return "Working..."
	}
}

// WindowContent is host-rendered secondary window content (plugins never draw UI).
type WindowContent struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Subtitle string          `json:"subtitle,omitempty"`
	IconHint string          `json:"iconHint,omitempty"`
	Sections []WindowSection `json:"sections"`
}

// WindowSection groups blocks inside a plugin window.
type WindowSection struct {
	ID     string        `json:"id"`
	Title  string        `json:"title,omitempty"`
	Blocks []WindowBlock `json:"blocks"`
}

// WindowBlock is a single rendered content block.
// Style: "paragraph" | "bullet" | "code" (empty defaults to paragraph).
type WindowBlock struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Style string `json:"style,omitempty"`
}

// Well-known window block styles.
const (
	WindowBlockParagraph = "paragraph"
	WindowBlockBullet    = "bullet"
	WindowBlockCode      = "code"
	WindowBlockMarkdown  = "markdown"
)

// ActionOK builds a successful ActionResult with optional data.
func ActionOK(data string) ActionResult {
	return ActionResult{Success: true, Data: data}
}

// ActionFail builds a failed ActionResult with an error message.
func ActionFail(err string) ActionResult {
	return ActionResult{Success: false, Error: err}
}

// ActionWindow builds a successful ActionResult that opens a host window.
func ActionWindow(window WindowContent) ActionResult {
	return ActionResult{Success: true, Window: &window}
}

// ActionAIWindow opens a host window and requests a local AI assist update.
func ActionAIWindow(window WindowContent, ai AIAssist) ActionResult {
	return ActionResult{Success: true, Window: &window, AI: &ai}
}

// AIWindowOpts configures a loading window + AI assist request.
type AIWindowOpts struct {
	ID           string
	Title        string
	Subtitle     string
	IconHint     string
	Task         string
	Input        string
	SectionTitle string
}

// ActionAITask builds a loading window and attaches a host AI assist request.
func ActionAITask(opts AIWindowOpts) ActionResult {
	sectionTitle := opts.SectionTitle
	if sectionTitle == "" {
		sectionTitle = AITaskSectionTitle(opts.Task)
	}
	icon := opts.IconHint
	if icon == "" {
		icon = "doc.text"
	}
	window := WindowContent{
		ID:       opts.ID,
		Title:    opts.Title,
		Subtitle: opts.Subtitle,
		IconHint: icon,
		Sections: []WindowSection{{
			ID:    "loading",
			Title: sectionTitle,
			Blocks: []WindowBlock{{
				ID:    "generating",
				Text:  AITaskLoadingLabel(opts.Task),
				Style: WindowBlockParagraph,
			}},
		}},
	}
	return ActionAIWindow(window, AIAssist{
		Task:         opts.Task,
		Input:        opts.Input,
		WindowID:     opts.ID,
		SectionTitle: sectionTitle,
	})
}

// Alert is a top-of-menu warning/info banner.
type Alert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Event is an unsolicited notification emitted by the plugin.
type Event struct {
	Type      string            `json:"type"`
	PluginID  string            `json:"pluginId"`
	Message   string            `json:"message"`
	Severity  string            `json:"severity"`
	Data      map[string]string `json:"data"`
	Timestamp string            `json:"timestamp"`
}

// Well-known values for state / severity / trend / health fields.
const (
	StateReady    = "ready"
	StateLoading  = "loading"
	StateDegraded = "degraded"
	StateError    = "error"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"

	TrendUp     = "up"
	TrendDown   = "down"
	TrendSteady = "steady"

	HealthOK          = "ok"
	HealthDegraded    = "degraded"
	HealthError       = "error"
	HealthRateLimited = "rate_limited"
	HealthAuthReq     = "auth_required"
)
