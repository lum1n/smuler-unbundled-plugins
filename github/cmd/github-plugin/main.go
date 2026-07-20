package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	"github.com/lum1n/smuler/plugins/plugindebug"
)

func logDebug(format string, args ...interface{}) {
	plugindebug.Log("[github-plugin]", format, args...)
}

const (
	protocolVersion = "0.1.0"
	pluginVersion   = "0.1.0"
	apiBase         = "https://api.github.com"
)

// --- JSON-RPC types ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    *pluginErrorData `json:"data,omitempty"`
}

type pluginErrorData struct {
	Retryable       bool   `json:"retryable"`
	SuggestedAction string `json:"suggestedAction,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string                `json:"protocolVersion"`
	PluginID        string                `json:"pluginId"`
	Config          map[string]string     `json:"config"`
	Auth            *pluginAuthContext    `json:"auth,omitempty"`
	ProviderAuths   []providerAuthContext `json:"providerAuths,omitempty"`
}

type pluginAuthContext struct {
	AccountID string `json:"accountId"`
}

type providerAuthContext struct {
	ProviderID  string `json:"providerId"`
	Kind        string `json:"kind"`
	AccountID   string `json:"accountId,omitempty"`
	AccessToken string `json:"accessToken,omitempty"`
}

type initializedPayload struct {
	Type            string `json:"type"`
	ProtocolVersion string `json:"protocolVersion"`
	PluginVersion   string `json:"pluginVersion"`
	Health          string `json:"health"`
}

type pluginSnapshot struct {
	PluginID     string         `json:"pluginId"`
	State        string         `json:"state"`
	Summary      pluginSummary  `json:"summary"`
	Items        []pluginItem   `json:"items"`
	Actions      []pluginAction `json:"actions"`
	Alerts       []pluginAlert  `json:"alerts"`
	RefreshAfter int            `json:"refreshAfter"`
	Health       string         `json:"health"`
}

type pluginSummary struct {
	Title    string `json:"title"`
	Value    string `json:"value"`
	Trend    string `json:"trend"`
	Severity string `json:"severity"`
	IconHint string `json:"iconHint"`
}

type pluginItem struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Subtitle  string         `json:"subtitle,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	Severity  string         `json:"severity"`
	Timestamp string         `json:"timestamp,omitempty"`
	DeepLink  string         `json:"deepLink,omitempty"`
	Actions   []pluginAction `json:"actions"`
}

type pluginAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type pluginAlert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type pluginEvent struct {
	Type      string            `json:"type"`
	PluginID  string            `json:"pluginId"`
	Message   string            `json:"message"`
	Severity  string            `json:"severity"`
	Data      map[string]string `json:"data"`
	Timestamp string            `json:"timestamp"`
}

type eventParams struct {
	Event pluginEvent `json:"event"`
}

// --- GitHub API types ---

type ghSearchResponse struct {
	TotalCount int      `json:"total_count"`
	Items      []ghPR   `json:"items"`
}

type ghPR struct {
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
}

type githubPlugin struct {
	token  string
	client *http.Client
	apiBase string

	prevPRs         map[string]prevPRInfo // pr-N -> info
	prevReviewCount int
	prMetaMap       map[string]prMeta // itemID -> metadata for getMRDiff
}

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
	StatusCode int
	Body       []byte
	RetryAfter int
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
	req.Header.Set("User-Agent", "smuler-github-plugin/0.1.0")
	if p.token != "" {
		req.Header.Set("Authorization", "token "+p.token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return httpResponse{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return httpResponse{}, err
	}

	return httpResponse{
		StatusCode: resp.StatusCode,
		Body:       body,
		RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
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

func (p *githubPlugin) buildSnapshot() pluginSnapshot {
	if p.token == "" {
		return pluginSnapshot{
			PluginID:     "github",
			State:        "degraded",
			Summary:      pluginSummary{Title: "GitHub", Value: "No token configured", Trend: "steady", Severity: "info", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "github-config", Severity: "info", Message: "No token configured"}},
			RefreshAfter: 300,
			Health:       "auth_required",
		}
	}

	body, err := p.apiRequest("/search/issues?q=is:pr+is:open+review-requested:@me+archived:false&per_page=5&sort=updated&order=desc")
	if err != nil {
		msg := fmt.Sprintf("API unreachable: %v", err)
		return pluginSnapshot{
			PluginID:     "github",
			State:        "degraded",
			Summary:      pluginSummary{Title: "GitHub", Value: msg, Trend: "steady", Severity: "info", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "github-error", Severity: "info", Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(httphealth.HealthDegraded, 0),
			Health:       httphealth.HealthDegraded,
		}
	}

	if body.StatusCode == http.StatusUnauthorized || body.StatusCode == http.StatusForbidden {
		msg := "GitHub authentication failed — reconnect in Settings"
		return pluginSnapshot{
			PluginID:     "github",
			State:        "error",
			Summary:      pluginSummary{Title: "GitHub", Value: "Auth required", Trend: "steady", Severity: "warning", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "github-auth", Severity: "warning", Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(httphealth.HealthAuthReq, body.RetryAfter),
			Health:       httphealth.HealthAuthReq,
		}
	}

	if body.StatusCode == http.StatusTooManyRequests {
		msg := "GitHub API rate limited"
		return pluginSnapshot{
			PluginID:     "github",
			State:        "degraded",
			Summary:      pluginSummary{Title: "GitHub", Value: "Rate limited", Trend: "steady", Severity: "warning", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "github-rate-limit", Severity: "warning", Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(httphealth.HealthRateLimited, body.RetryAfter),
			Health:       httphealth.HealthRateLimited,
		}
	}

	if body.StatusCode < 200 || body.StatusCode >= 300 {
		msg := fmt.Sprintf("GitHub API error (%d)", body.StatusCode)
		health := httphealth.ClassifyHTTPStatus(body.StatusCode)
		return pluginSnapshot{
			PluginID:     "github",
			State:        "degraded",
			Summary:      pluginSummary{Title: "GitHub", Value: msg, Trend: "steady", Severity: "warning", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "github-error", Severity: "warning", Message: msg}},
			RefreshAfter: httphealth.DefaultRefreshAfter(health, body.RetryAfter),
			Health:       health,
		}
	}

	var searchResp ghSearchResponse
	if err := json.Unmarshal(body.Body, &searchResp); err != nil {
		msg := fmt.Sprintf("Failed to parse API response: %v", err)
		return pluginSnapshot{
			PluginID:     "github",
			State:        "degraded",
			Summary:      pluginSummary{Title: "GitHub", Value: msg, Trend: "steady", Severity: "warning", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "github-parse", Severity: "warning", Message: msg}},
			RefreshAfter: 120,
			Health:       "degraded",
		}
	}

	reviewCount := searchResp.TotalCount
	itemsData := searchResp.Items

	p.emitDeltaEvents(itemsData, reviewCount)

	var severity string
	var value string
	if reviewCount > 0 {
		severity = "warning"
		s := "s"
		if reviewCount == 1 {
			s = ""
		}
		value = fmt.Sprintf("%d review%s", reviewCount, s)
	} else {
		severity = "info"
		value = "No reviews"
	}

	items := make([]pluginItem, 0, 3)
	for _, pr := range itemsData {
		if len(items) >= 3 {
			break
		}
		repo := strings.TrimPrefix(pr.RepositoryURL, apiBase+"/repos/")
		itemID := fmt.Sprintf("pr-%d", pr.Number)
		p.prMetaMap[itemID] = prMeta{
			number: pr.Number,
			repo:   repo,
		}
		items = append(items, pluginItem{
			ID:        itemID,
			Title:     pr.Title,
			Subtitle:  repo,
			Detail:    fmt.Sprintf("#%d — %s", pr.Number, pr.User.Login),
			Severity:  severity,
			Timestamp: pr.CreatedAt,
			DeepLink:  pr.HTMLURL,
			Actions:   []pluginAction{{ID: "open", Label: "Open PR"}, {ID: "summarize", Label: "Summarize"}},
		})
	}

	alerts := make([]pluginAlert, 0)
	if reviewCount > 0 {
		alerts = append(alerts, pluginAlert{
			ID:       "review-backlog",
			Severity: "warning",
			Message:  fmt.Sprintf("%d review request(s) need attention", reviewCount),
		})
	}

	return pluginSnapshot{
		PluginID:     "github",
		State:        "ready",
		Summary:      pluginSummary{Title: "GitHub", Value: value, Trend: "steady", Severity: severity, IconHint: "pull-request"},
		Items:        items,
		Actions:      []pluginAction{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: 300,
		Health:       "ok",
	}
}

func (p *githubPlugin) emitDeltaEvents(items []ghPR, reviewCount int) {
	currentPRs := make(map[string]prevPRInfo)
	for _, pr := range items {
		key := fmt.Sprintf("pr-%d", pr.Number)
		currentPRs[key] = prevPRInfo{title: pr.Title, url: pr.HTMLURL}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// PRs that disappeared from review-requested list
	for key, info := range p.prevPRs {
		if _, ok := currentPRs[key]; !ok {
			p.emitEvent(pluginEvent{
				Type:     "pr.review_completed",
				PluginID: "github",
				Message:  fmt.Sprintf("Review completed for %s", key),
				Severity: "info",
				Data:     map[string]string{"prId": key, "url": info.url},
				Timestamp: now,
			})
		}
	}

	// New PRs added to review-requested list
	for key, info := range currentPRs {
		if _, ok := p.prevPRs[key]; !ok && len(p.prevPRs) > 0 {
			p.emitEvent(pluginEvent{
				Type:     "pr.review_requested",
				PluginID: "github",
				Message:  fmt.Sprintf("Review requested: %s", info.title),
				Severity: "warning",
				Data:     map[string]string{"prId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	// Review count changed
	if reviewCount != p.prevReviewCount && len(p.prevPRs) > 0 {
		if reviewCount > p.prevReviewCount {
			p.emitEvent(pluginEvent{
				Type:     "pr.count_increased",
				PluginID: "github",
				Message:  fmt.Sprintf("Review requests increased from %d to %d", p.prevReviewCount, reviewCount),
				Severity: "warning",
				Data: map[string]string{
					"previous": strconv.Itoa(p.prevReviewCount),
					"current":  strconv.Itoa(reviewCount),
				},
				Timestamp: now,
			})
		} else if reviewCount < p.prevReviewCount {
			p.emitEvent(pluginEvent{
				Type:     "pr.count_decreased",
				PluginID: "github",
				Message:  fmt.Sprintf("Review requests decreased from %d to %d", p.prevReviewCount, reviewCount),
				Severity: "info",
				Data: map[string]string{
					"previous": strconv.Itoa(p.prevReviewCount),
					"current":  strconv.Itoa(reviewCount),
				},
				Timestamp: now,
			})
		}
	}

	p.prevPRs = currentPRs
	p.prevReviewCount = reviewCount
}

func (p *githubPlugin) emitEvent(event pluginEvent) {
	event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	params := eventParams{Event: event}
	payload, err := json.Marshal(params)
	if err != nil {
		logDebug("json marshal error: %v", err)
		return
	}
	out := fmt.Sprintf(`{"jsonrpc":"2.0","method":"event","params":%s}`, string(payload))
	fmt.Fprintf(os.Stdout, "%s\n", out)
}

func sendResult(id int, result interface{}) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	data, err := json.Marshal(resp)
	if err != nil {
		logDebug("json marshal error: %v", err)
		return
	}
	fmt.Fprintf(os.Stdout, "%s\n", string(data))
}

func sendError(id int, code int, message string, retryable bool, suggestedAction string) {
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    &pluginErrorData{Retryable: retryable, SuggestedAction: suggestedAction},
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		logDebug("json marshal error: %v", err)
		return
	}
	fmt.Fprintf(os.Stdout, "%s\n", string(data))
}

var initResult = initializedPayload{
	Type:            "initialized",
	ProtocolVersion: protocolVersion,
	PluginVersion:   pluginVersion,
	Health:          "ok",
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			logDebug("panic: %v", r)
		}
	}()

	pl := &githubPlugin{
		client:  &http.Client{Timeout: 12 * time.Second},
		prevPRs: make(map[string]prevPRInfo),
		prMetaMap: make(map[string]prMeta),
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(nil, 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			var params initializeParams
			if err := json.Unmarshal(req.Params, &params); err == nil {
				plugindebug.ConfigureFromInitializeConfig(params.Config)
			for _, auth := range params.ProviderAuths {
				if auth.AccountID != "" {
					pl.token = auth.AccountID
					break
				}
				if auth.AccessToken != "" {
					pl.token = auth.AccessToken
					break
				}
			}
			if pl.token == "" && params.Auth != nil && params.Auth.AccountID != "" {
				pl.token = params.Auth.AccountID
			}
		}
			sendResult(req.ID, initResult)

		case "getStatus":
			sendResult(req.ID, pl.buildSnapshot())

		case "refresh":
			sendResult(req.ID, pl.buildSnapshot())

		case "shutdown":
			sendResult(req.ID, nil)
			return

		case "performAction":
			var actionParams struct {
				ActionID string            `json:"actionId"`
				Payload  map[string]string `json:"payload"`
			}
			if err := json.Unmarshal(req.Params, &actionParams); err != nil {
				sendError(req.ID, -32602, "invalid params", false, "")
				continue
			}
			switch actionParams.ActionID {
			case "getMRDiff":
				idStr := strings.TrimSpace(actionParams.Payload["id"])
				success, data := pl.handleGetMRDiff(idStr)
				sendResult(req.ID, struct {
					Success bool   `json:"success"`
					Data    string `json:"data,omitempty"`
				}{success, data})
			default:
				sendError(req.ID, -32601, fmt.Sprintf("unknown action: %s", actionParams.ActionID), false, "")
			}

		default:
			sendError(req.ID, -32601, fmt.Sprintf("unknown method: %s", req.Method), false, "")
		}
	}

	if err := scanner.Err(); err != nil {
		logDebug("stdin scanner error: %v", err)
	}
}
