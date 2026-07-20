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
