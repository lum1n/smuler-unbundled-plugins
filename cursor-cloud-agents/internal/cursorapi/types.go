package cursorapi

import "time"

const DefaultBaseURL = "https://api.cursor.com"

// MeResponse is returned by GET /v1/me.
type MeResponse struct {
	APIKeyName    string `json:"apiKeyName"`
	CreatedAt     string `json:"createdAt"`
	UserID        int    `json:"userId"`
	UserEmail     string `json:"userEmail"`
	UserFirstName string `json:"userFirstName"`
	UserLastName  string `json:"userLastName"`
}

// AgentListItem is a summary row from GET /v1/agents.
type AgentListItem struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	Env         AgentEnv `json:"env"`
	URL         string   `json:"url"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
	LatestRunID string   `json:"latestRunId"`
}

// AgentEnv describes where an agent runs.
type AgentEnv struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// AgentDetail is the full agent record from GET /v1/agents/{id}.
type AgentDetail struct {
	AgentListItem
	Repos               []AgentRepo `json:"repos"`
	WorkOnCurrentBranch bool        `json:"workOnCurrentBranch"`
	AutoCreatePR        bool        `json:"autoCreatePR"`
}

// AgentRepo is a repository configuration on an agent.
type AgentRepo struct {
	URL         string `json:"url"`
	StartingRef string `json:"startingRef,omitempty"`
	PRURL       string `json:"prUrl,omitempty"`
}

// AgentListResponse is returned by GET /v1/agents.
type AgentListResponse struct {
	Items      []AgentListItem `json:"items"`
	NextCursor string          `json:"nextCursor"`
}

// RunDetail is returned by GET /v1/agents/{id}/runs/{runId}.
type RunDetail struct {
	ID         string `json:"id"`
	AgentID    string `json:"agentId"`
	Status     string `json:"status"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
	DurationMs int    `json:"durationMs"`
	Result     string `json:"result"`
	Git        RunGit `json:"git"`
}

// RunGit holds pushed branches for an agent run.
type RunGit struct {
	Branches []GitBranch `json:"branches"`
}

// GitBranch is one pushed branch (and optional PR).
type GitBranch struct {
	RepoURL string `json:"repoUrl"`
	Branch  string `json:"branch,omitempty"`
	PRURL   string `json:"prUrl,omitempty"`
}

// TokenUsage counts token consumption.
type TokenUsage struct {
	InputTokens      int `json:"inputTokens"`
	OutputTokens     int `json:"outputTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens"`
	CacheReadTokens  int `json:"cacheReadTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// AgentUsageResponse is returned by GET /v1/agents/{id}/usage.
type AgentUsageResponse struct {
	TotalUsage TokenUsage      `json:"totalUsage"`
	Runs       []RunUsageEntry `json:"runs"`
}

// RunUsageEntry is per-run usage inside AgentUsageResponse.
type RunUsageEntry struct {
	ID        string     `json:"id"`
	UsageUUID string     `json:"usageUuid,omitempty"`
	Usage     TokenUsage `json:"usage"`
}

// EnrichedAgent combines list data with optional detail fetches.
type EnrichedAgent struct {
	List   AgentListItem
	Detail *AgentDetail
	Run    *RunDetail
	Usage  *AgentUsageResponse
}

// ListOptions controls agent listing.
type ListOptions struct {
	IncludeArchived bool
	PageLimit       int
}

// EnrichOptions controls per-agent detail fetches.
type EnrichOptions struct {
	DetailLevel string
	MaxAgents   int
	Concurrency int
}

// APIError represents a non-2xx API response.
type APIError struct {
	StatusCode int
	Body       string
	// RetryAfter is the parsed Retry-After header in seconds (0 if absent).
	RetryAfter int
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return e.Body
	}
	return "cursor api error"
}

func (e *APIError) IsAuth() bool {
	return e.StatusCode == 401 || e.StatusCode == 403
}

func (e *APIError) IsRateLimited() bool {
	return e.StatusCode == 429
}

// ParseTime parses ISO-8601 timestamps from the API.
func ParseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return time.Time{}
		}
	}
	return t
}
