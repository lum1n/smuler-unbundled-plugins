package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "github"
	pluginVersion = "0.1.2"
	apiBase       = "https://api.github.com"

	// searchPageSize is fetched in one request so delta events see the whole
	// review queue (not just the displayed rows).
	searchPageSize  = 50
	maxDisplayItems = 3
)

// --- GitHub API types ---

type ghSearchResponse struct {
	TotalCount int    `json:"total_count"`
	Items      []ghPR `json:"items"`
}

type ghPR struct {
	ID            int64  `json:"id"`
	Number        int    `json:"number"`
	Title         string `json:"title"`
	HTMLURL       string `json:"html_url"`
	RepositoryURL string `json:"repository_url"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	User          ghUser `json:"user"`
	State         string `json:"state"`
	Draft         bool   `json:"draft"`
}

type ghUser struct {
	Login string `json:"login"`
}

// --- Plugin state ---

type prevPRInfo struct {
	title string
	url   string
}

type prMeta struct {
	number int
	repo   string // owner/repo
	title  string
}

type githubPlugin struct {
	token   string
	client  *http.Client
	apiBase string

	prevPRs         map[string]prevPRInfo // itemID -> info
	prevReviewCount int
	seeded          bool              // true once a successful snapshot has populated prevPRs
	prMetaMap       map[string]prMeta // itemID -> metadata for getMRDiff
}

func (p *githubPlugin) Initialize(params sdk.InitializeParams) string {
	p.token = resolveToken(params)
	if p.token == "" {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

// resolveToken picks the GitHub token from initialize params. Provider auth
// records carry the secret in accessToken (OAuth) or apiKey; their accountId is
// only a record identifier (often a UUID), so it is used as a last resort for
// older hosts that stored the token there. The legacy single-token auth
// context stores the raw token in accountId.
func resolveToken(params sdk.InitializeParams) string {
	for _, auth := range params.ProviderAuths {
		if t := strings.TrimSpace(auth.AccessToken); t != "" {
			return t
		}
		if t := strings.TrimSpace(auth.APIKey); t != "" {
			return t
		}
	}
	if params.Auth != nil {
		if t := strings.TrimSpace(params.Auth.AccountID); t != "" {
			return t
		}
	}
	for _, auth := range params.ProviderAuths {
		if t := strings.TrimSpace(auth.AccountID); t != "" {
			return t
		}
	}
	return ""
}

func (p *githubPlugin) GetStatus() sdk.Snapshot {
	return p.buildSnapshot()
}

func (p *githubPlugin) PerformAction(id string, params map[string]string) (bool, string) {
	return false, "use PerformActionResult"
}

func (p *githubPlugin) PerformActionResult(id string, params map[string]string) sdk.ActionResult {
	switch id {
	case "getMRDiff":
		success, data := p.handleGetMRDiff(strings.TrimSpace(params["id"]))
		if !success {
			return sdk.ActionFail(data)
		}
		return sdk.ActionOK(data)
	case "summarize":
		return p.handleSummarize(strings.TrimSpace(params["id"]))
	default:
		return sdk.ActionFail(fmt.Sprintf("unknown action: %s", id))
	}
}

func (p *githubPlugin) Shutdown() {}

func (p *githubPlugin) apiBaseURL() string {
	if p.apiBase != "" {
		return p.apiBase
	}
	return apiBase
}

func (p *githubPlugin) apiRequest(path string) (httpResponse, error) {
	return p.apiRequestRaw(path, "application/vnd.github.v3+json")
}

type httpResponse struct {
	StatusCode  int
	Body        []byte
	RetryAfter  int
	RateLimited bool
}

// maxBodyBytes caps how much of a response is read (diffs can be very large).
const maxBodyBytes = 4 << 20

// rateLimitStatus reports whether a GitHub response is a primary or secondary
// rate limit and how many seconds to wait. GitHub signals primary limits with
// 403/429 + X-RateLimit-Remaining: 0 (reset in X-RateLimit-Reset) and
// secondary limits with 403/429 + Retry-After.
func rateLimitStatus(statusCode int, h http.Header, body []byte) (bool, int) {
	retryAfter := httphealth.ParseRetryAfter(h.Get("Retry-After"))
	if retryAfter == 0 && h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Until(time.Unix(reset, 0)); d > 0 {
				retryAfter = int(d.Seconds()) + 1
			}
		}
	}
	switch statusCode {
	case http.StatusTooManyRequests:
		return true, retryAfter
	case http.StatusForbidden:
		if h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(string(body)), "rate limit") {
			return true, retryAfter
		}
	}
	return false, retryAfter
}

func (p *githubPlugin) apiRequestRaw(path string, acceptHeader string) (httpResponse, error) {
	req, err := http.NewRequest("GET", p.apiBaseURL()+path, nil)
	if err != nil {
		return httpResponse{}, err
	}
	if acceptHeader != "" {
		req.Header.Set("Accept", acceptHeader)
	} else {
		req.Header.Set("Accept", "application/vnd.github.v3+json")
	}
	req.Header.Set("User-Agent", "smuler-github-plugin/"+pluginVersion)
	if p.token != "" {
		req.Header.Set("Authorization", "token "+p.token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return httpResponse{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return httpResponse{}, err
	}
	// Drain any remainder so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)

	rateLimited, retryAfter := rateLimitStatus(resp.StatusCode, resp.Header, body)
	return httpResponse{
		StatusCode:  resp.StatusCode,
		Body:        body,
		RetryAfter:  retryAfter,
		RateLimited: rateLimited,
	}, nil
}

func (p *githubPlugin) fetchPRDiff(repo string, number int) (string, error) {
	resp, err := p.apiRequestRaw(
		fmt.Sprintf("/repos/%s/pulls/%d", repo, number),
		"application/vnd.github.v3.diff",
	)
	if err != nil {
		return "", fmt.Errorf("failed to fetch PR diff: %w", err)
	}
	if resp.RateLimited {
		return "", fmt.Errorf("GitHub API rate limited")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(resp.Body), nil
}

func (p *githubPlugin) handleGetMRDiff(idStr string) (bool, string) {
	meta, ok := p.prMetaMap[idStr]
	if !ok {
		return false, fmt.Sprintf("PR not found in cache: %s", idStr)
	}

	diff, err := p.fetchPRDiff(meta.repo, meta.number)
	if err != nil {
		return false, fmt.Sprintf("Failed to fetch diff: %v", err)
	}
	return true, diff
}

func (p *githubPlugin) handleSummarize(idStr string) sdk.ActionResult {
	meta, ok := p.prMetaMap[idStr]
	if !ok {
		return sdk.ActionFail(fmt.Sprintf("PR not found in cache: %s", idStr))
	}

	diff, err := p.fetchPRDiff(meta.repo, meta.number)
	if err != nil {
		return sdk.ActionFail(fmt.Sprintf("Failed to fetch diff: %v", err))
	}
	if strings.TrimSpace(diff) == "" {
		return sdk.ActionFail("Empty diff")
	}

	title := meta.title
	if title == "" {
		title = fmt.Sprintf("PR #%d", meta.number)
	}
	return sdk.ActionAITask(sdk.AIWindowOpts{
		ID:       fmt.Sprintf("github.summarize.%s", idStr),
		Title:    title,
		Subtitle: "GitHub",
		IconHint: "arrow.triangle.pull",
		Task:     sdk.AITaskSummarizeBullets,
		Input:    diff,
	})
}

func (p *githubPlugin) buildSnapshot() sdk.Snapshot {
	if p.token == "" {
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title: "GitHub", Value: "No token configured", Trend: sdk.TrendSteady,
				Severity: sdk.SeverityInfo, IconHint: "pull-request",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "github-config", Severity: sdk.SeverityInfo, Message: "No token configured"}},
			RefreshAfter: 300,
			Health:       sdk.HealthAuthReq,
		}
	}

	body, err := p.apiRequest("/search/issues?q=is:pr+is:open+review-requested:@me+archived:false&per_page=" + strconv.Itoa(searchPageSize) + "&sort=updated&order=desc")
	if err != nil {
		msg := fmt.Sprintf("API unreachable: %v", err)
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title: "GitHub", Value: msg, Trend: sdk.TrendSteady,
				Severity: sdk.SeverityInfo, IconHint: "pull-request",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "github-error", Severity: sdk.SeverityInfo, Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(httphealth.HealthDegraded, 0),
			Health:       httphealth.HealthDegraded,
		}
	}

	if body.RateLimited {
		msg := "GitHub API rate limited"
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title: "GitHub", Value: "Rate limited", Trend: sdk.TrendSteady,
				Severity: sdk.SeverityWarning, IconHint: "pull-request",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "github-rate-limit", Severity: sdk.SeverityWarning, Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(httphealth.HealthRateLimited, body.RetryAfter),
			Health:       httphealth.HealthRateLimited,
		}
	}

	if body.StatusCode == http.StatusUnauthorized || body.StatusCode == http.StatusForbidden {
		msg := "GitHub authentication failed — reconnect in Settings"
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateError,
			Summary: sdk.Summary{
				Title: "GitHub", Value: "Auth required", Trend: sdk.TrendSteady,
				Severity: sdk.SeverityWarning, IconHint: "pull-request",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "github-auth", Severity: sdk.SeverityWarning, Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(httphealth.HealthAuthReq, body.RetryAfter),
			Health:       httphealth.HealthAuthReq,
		}
	}

	if body.StatusCode < 200 || body.StatusCode >= 300 {
		msg := fmt.Sprintf("GitHub API error (%d)", body.StatusCode)
		health := httphealth.ClassifyHTTPStatus(body.StatusCode)
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title: "GitHub", Value: msg, Trend: sdk.TrendSteady,
				Severity: sdk.SeverityWarning, IconHint: "pull-request",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "github-error", Severity: sdk.SeverityWarning, Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(health, body.RetryAfter),
			Health:       health,
		}
	}

	var searchResp ghSearchResponse
	if err := json.Unmarshal(body.Body, &searchResp); err != nil {
		msg := fmt.Sprintf("Failed to parse API response: %v", err)
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title: "GitHub", Value: msg, Trend: sdk.TrendSteady,
				Severity: sdk.SeverityWarning, IconHint: "pull-request",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "github-parse", Severity: sdk.SeverityWarning, Message: msg}},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	reviewCount := searchResp.TotalCount
	itemsData := searchResp.Items

	p.emitDeltaEvents(itemsData, reviewCount)

	var severity string
	var value string
	if reviewCount > 0 {
		severity = sdk.SeverityWarning
		s := "s"
		if reviewCount == 1 {
			s = ""
		}
		value = fmt.Sprintf("%d review%s", reviewCount, s)
	} else {
		severity = sdk.SeverityInfo
		value = "No reviews"
	}

	// Rebuild the action lookup from the current snapshot so it stays bounded.
	p.prMetaMap = make(map[string]prMeta, maxDisplayItems)
	items := make([]sdk.Item, 0, maxDisplayItems)
	for _, pr := range itemsData {
		if len(items) >= maxDisplayItems {
			break
		}
		repo := p.repoName(pr)
		itemID := prItemID(pr)
		p.prMetaMap[itemID] = prMeta{
			number: pr.Number,
			repo:   repo,
			title:  pr.Title,
		}
		items = append(items, sdk.Item{
			ID:        itemID,
			Title:     pr.Title,
			Subtitle:  repo,
			Detail:    fmt.Sprintf("#%d — %s", pr.Number, pr.User.Login),
			Severity:  severity,
			Timestamp: pr.CreatedAt,
			DeepLink:  pr.HTMLURL,
			Actions:   []sdk.Action{{ID: "open", Label: "Open PR"}, {ID: "summarize", Label: "Summarize"}},
		})
	}

	alerts := make([]sdk.Alert, 0)
	if reviewCount > 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "review-backlog",
			Severity: sdk.SeverityWarning,
			Message:  fmt.Sprintf("%d review request(s) need attention", reviewCount),
		})
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateReady,
		Summary: sdk.Summary{
			Title: "GitHub", Value: value, Trend: sdk.TrendSteady,
			Severity: severity, IconHint: "pull-request",
		},
		Items:        items,
		Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: 300,
		Health:       sdk.HealthOK,
	}
}

// prItemID returns a stable, globally unique item id. PR numbers alone collide
// across repositories, so the search API's global issue id is used.
func prItemID(pr ghPR) string {
	if pr.ID != 0 {
		return fmt.Sprintf("pr-%d", pr.ID)
	}
	return fmt.Sprintf("pr-%d", pr.Number)
}

func (p *githubPlugin) repoName(pr ghPR) string {
	repo := strings.TrimPrefix(pr.RepositoryURL, p.apiBaseURL()+"/repos/")
	return strings.TrimPrefix(repo, apiBase+"/repos/")
}

func (p *githubPlugin) emitDeltaEvents(items []ghPR, reviewCount int) {
	currentPRs := make(map[string]prevPRInfo)
	for _, pr := range items {
		currentPRs[prItemID(pr)] = prevPRInfo{title: pr.Title, url: pr.HTMLURL}
	}

	// Only items fetched in this page are known; when the backlog exceeds the
	// page, PRs missing from it may simply have been pushed out, so don't
	// report them as completed.
	complete := len(items) >= reviewCount

	if !p.seeded {
		p.prevPRs = currentPRs
		p.prevReviewCount = reviewCount
		p.seeded = true
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for key, info := range p.prevPRs {
		if _, still := currentPRs[key]; !still && complete {
			sdk.Emit(sdk.Event{
				Type:      "pr.review_completed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Review completed: %s", info.title),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"prId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	for key, info := range currentPRs {
		if _, was := p.prevPRs[key]; !was {
			sdk.Emit(sdk.Event{
				Type:      "pr.review_requested",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Review requested: %s", info.title),
				Severity:  sdk.SeverityWarning,
				Data:      map[string]string{"prId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	if reviewCount != p.prevReviewCount {
		if reviewCount > p.prevReviewCount {
			sdk.Emit(sdk.Event{
				Type:     "pr.count_increased",
				PluginID: pluginID,
				Message:  fmt.Sprintf("Review requests increased from %d to %d", p.prevReviewCount, reviewCount),
				Severity: sdk.SeverityWarning,
				Data: map[string]string{
					"previous": strconv.Itoa(p.prevReviewCount),
					"current":  strconv.Itoa(reviewCount),
				},
				Timestamp: now,
			})
		} else if reviewCount < p.prevReviewCount {
			sdk.Emit(sdk.Event{
				Type:     "pr.count_decreased",
				PluginID: pluginID,
				Message:  fmt.Sprintf("Review requests decreased from %d to %d", p.prevReviewCount, reviewCount),
				Severity: sdk.SeverityInfo,
				Data: map[string]string{
					"previous": strconv.Itoa(p.prevReviewCount),
					"current":  strconv.Itoa(reviewCount),
				},
				Timestamp: now,
			})
		}
	}

	if !complete {
		// Remember PRs that fell off the page so they don't re-fire later.
		for key, info := range p.prevPRs {
			if _, ok := currentPRs[key]; !ok {
				currentPRs[key] = info
			}
		}
	}
	p.prevPRs = currentPRs
	p.prevReviewCount = reviewCount
}

func main() {
	sdk.Run(pluginID, pluginVersion, &githubPlugin{
		client:    &http.Client{Timeout: 12 * time.Second},
		prevPRs:   make(map[string]prevPRInfo),
		prMetaMap: make(map[string]prMeta),
	})
}
