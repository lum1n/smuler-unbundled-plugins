package cursorapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client calls the Cursor Cloud Agents API v1.
type Client struct {
	baseURL    string
	httpClient *http.Client
	apiKey     string

	cacheMu sync.Mutex
	cache   map[string]enrichCacheEntry
}

// enrichCacheEntry remembers per-agent detail fetched for a list row so an
// unchanged, settled agent is not re-fetched (3 requests) on every refresh.
type enrichCacheEntry struct {
	updatedAt   string
	latestRunID string
	enriched    EnrichedAgent
}

// maxListPages bounds pagination so a misbehaving cursor cannot loop forever.
const maxListPages = 50

// NewClient creates an API client. apiKey must be non-empty before requests.
func NewClient(apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 12 * time.Second}
	}
	return &Client{
		baseURL:    DefaultBaseURL,
		httpClient: httpClient,
		apiKey:     strings.TrimSpace(apiKey),
	}
}

// SetAPIKey updates the API key used for requests.
func (c *Client) SetAPIKey(apiKey string) {
	c.apiKey = strings.TrimSpace(apiKey)
	c.cacheMu.Lock()
	c.cache = nil
	c.cacheMu.Unlock()
}

// SetBaseURL overrides the API base URL (for tests).
func (c *Client) SetBaseURL(baseURL string) {
	c.baseURL = strings.TrimRight(baseURL, "/")
}
func (c *Client) Me(ctx context.Context) (*MeResponse, error) {
	var out MeResponse
	if err := c.getJSON(ctx, "/v1/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllAgents paginates through every agent accessible to the API key.
func (c *Client) ListAllAgents(ctx context.Context, opts ListOptions) ([]AgentListItem, error) {
	pageLimit := opts.PageLimit
	if pageLimit <= 0 || pageLimit > 100 {
		pageLimit = 100
	}

	var all []AgentListItem
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		q.Set("limit", fmt.Sprintf("%d", pageLimit))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if opts.IncludeArchived {
			q.Set("includeArchived", "true")
		} else {
			q.Set("includeArchived", "false")
		}

		var resp AgentListResponse
		if err := c.getJSON(ctx, "/v1/agents", q, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Items...)
		if resp.NextCursor == "" || seen[resp.NextCursor] {
			break
		}
		seen[resp.NextCursor] = true
		cursor = resp.NextCursor
	}
	return all, nil
}

// GetAgent returns durable metadata for one agent.
func (c *Client) GetAgent(ctx context.Context, id string) (*AgentDetail, error) {
	var out AgentDetail
	path := "/v1/agents/" + url.PathEscape(id)
	if err := c.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRun returns status and result for a specific run.
func (c *Client) GetRun(ctx context.Context, agentID, runID string) (*RunDetail, error) {
	var out RunDetail
	path := fmt.Sprintf("/v1/agents/%s/runs/%s", url.PathEscape(agentID), url.PathEscape(runID))
	if err := c.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUsage returns token usage for an agent.
func (c *Client) GetUsage(ctx context.Context, agentID string) (*AgentUsageResponse, error) {
	var out AgentUsageResponse
	path := "/v1/agents/" + url.PathEscape(agentID) + "/usage"
	if err := c.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EnrichAgents fetches full detail for up to maxAgents entries.
func (c *Client) EnrichAgents(ctx context.Context, agents []AgentListItem, opts EnrichOptions) []EnrichedAgent {
	if opts.DetailLevel != "full" || len(agents) == 0 {
		out := make([]EnrichedAgent, len(agents))
		for i, a := range agents {
			out[i] = EnrichedAgent{List: a}
		}
		return out
	}

	limit := opts.MaxAgents
	if limit <= 0 {
		limit = len(agents)
	}
	if limit > len(agents) {
		limit = len(agents)
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 4
	}

	out := make([]EnrichedAgent, len(agents))
	for i, a := range agents {
		out[i] = EnrichedAgent{List: a}
	}

	c.cacheMu.Lock()
	prevCache := c.cache
	c.cacheMu.Unlock()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	nextCache := make(map[string]enrichCacheEntry, limit)

	for i := 0; i < limit; i++ {
		agent := agents[i]
		if entry, ok := prevCache[agent.ID]; ok && entry.reusableFor(agent) {
			e := entry.enriched
			e.List = agent
			out[i] = e
			nextCache[agent.ID] = entry
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			agent := agents[idx]
			enriched := c.enrichOne(ctx, agent)

			mu.Lock()
			out[idx] = enriched
			if enriched.complete() {
				nextCache[agent.ID] = enrichCacheEntry{
					updatedAt:   agent.UpdatedAt,
					latestRunID: agent.LatestRunID,
					enriched:    enriched,
				}
			}
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	c.cacheMu.Lock()
	c.cache = nextCache
	c.cacheMu.Unlock()
	return out
}

// enrichOne fetches detail, latest run, and usage for one agent. Detail and
// usage are independent, and the run can start immediately when the list row
// already names the latest run, so the three requests overlap.
func (c *Client) enrichOne(ctx context.Context, agent AgentListItem) EnrichedAgent {
	enriched := EnrichedAgent{List: agent}
	var wg sync.WaitGroup

	fetchRun := func(runID string) {
		if runID == "" {
			return
		}
		if run, err := c.GetRun(ctx, agent.ID, runID); err == nil {
			enriched.Run = run
		}
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		if usage, err := c.GetUsage(ctx, agent.ID); err == nil {
			enriched.Usage = usage
		}
	}()
	if agent.LatestRunID != "" {
		go func() {
			defer wg.Done()
			fetchRun(agent.LatestRunID)
		}()
		if detail, err := c.GetAgent(ctx, agent.ID); err == nil {
			enriched.Detail = detail
		}
	} else {
		go func() {
			defer wg.Done()
			if detail, err := c.GetAgent(ctx, agent.ID); err == nil {
				enriched.Detail = detail
				fetchRun(detail.LatestRunID)
			}
		}()
	}
	wg.Wait()
	return enriched
}

// complete reports whether every detail fetch succeeded and the agent is in a
// settled state, i.e. its enrichment is safe to reuse while the list row is unchanged.
func (e EnrichedAgent) complete() bool {
	if e.Detail == nil || e.Usage == nil {
		return false
	}
	runID := e.List.LatestRunID
	if runID == "" {
		runID = e.Detail.LatestRunID
	}
	if runID != "" && e.Run == nil {
		return false
	}
	status := e.List.Status
	if e.Run != nil && e.Run.Status != "" {
		status = e.Run.Status
	}
	switch strings.ToUpper(status) {
	case "RUNNING", "CREATING", "ACTIVE":
		return false
	}
	return true
}

func (entry enrichCacheEntry) reusableFor(agent AgentListItem) bool {
	return agent.UpdatedAt != "" &&
		entry.updatedAt == agent.UpdatedAt &&
		entry.latestRunID == agent.LatestRunID
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, dest any) error {
	if c.apiKey == "" {
		return &APIError{StatusCode: 401, Body: "missing api key"}
	}

	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return err
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.apiKey, "")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "smuler-cursor-cloud-agents-plugin/0.1.0")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Body:       errorMessage(resp.Status, body),
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// errorMessage keeps API error text short and single-line so HTML error pages
// or large JSON bodies never end up verbatim in alerts.
func errorMessage(status string, body []byte) string {
	var parsed struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &parsed) == nil {
		msg = strings.TrimSpace(parsed.Message)
		if msg == "" {
			msg = strings.TrimSpace(parsed.Error)
		}
	}
	if msg == "" {
		return status
	}
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:199]) + "…"
	}
	return status + ": " + msg
}

func parseRetryAfter(header string) int {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return seconds
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return int(d.Seconds())
		}
	}
	return 0
}
