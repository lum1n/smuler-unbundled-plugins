package cursorapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client calls the Cursor Cloud Agents API v1.
type Client struct {
	baseURL    string
	httpClient *http.Client
	apiKey     string
}

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
	for {
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
		if resp.NextCursor == "" {
			break
		}
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

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	recordErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			agent := agents[idx]
			enriched := EnrichedAgent{List: agent}

			detail, err := c.GetAgent(ctx, agent.ID)
			if err != nil {
				recordErr(err)
			} else {
				enriched.Detail = detail
			}

			runID := agent.LatestRunID
			if enriched.Detail != nil && enriched.Detail.LatestRunID != "" {
				runID = enriched.Detail.LatestRunID
			}
			if runID != "" {
				run, err := c.GetRun(ctx, agent.ID, runID)
				if err != nil {
					recordErr(err)
				} else {
					enriched.Run = run
				}
			}

			usage, err := c.GetUsage(ctx, agent.ID)
			if err != nil {
				recordErr(err)
			} else {
				enriched.Usage = usage
			}

			mu.Lock()
			out[idx] = enriched
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	return out
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
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return &APIError{StatusCode: resp.StatusCode, Body: msg}
	}

	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(body, dest); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
