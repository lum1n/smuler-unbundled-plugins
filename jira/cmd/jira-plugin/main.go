package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	"github.com/lum1n/smuler/plugins/plugindebug"
)

func logDebug(format string, args ...interface{}) {
	plugindebug.Log("[jira-plugin]", format, args...)
}

const (
	protocolVersion = "0.1.0"
	pluginVersion   = "0.1.0"
)

type apiHTTPError struct {
	StatusCode int
	RetryAfter int
	Body       string
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

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
	ProviderAuths   []providerAuthContext `json:"providerAuths"`
}

type providerAuthContext struct {
	ProviderID   string `json:"providerId"`
	Kind         string `json:"kind"`
	CookieHeader string `json:"cookieHeader,omitempty"`
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

type performActionParams struct {
	PluginID string            `json:"pluginId"`
	ActionID string            `json:"actionId"`
	Payload  map[string]string `json:"payload"`
}

type performActionResult struct {
	Success bool   `json:"success"`
	Data    string `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// --- Jira API types ---

type jiraSearchResponse struct {
	Issues []jiraIssue `json:"issues"`
	Total  int         `json:"total"`
}

type jiraIssue struct {
	ID     string        `json:"id"`
	Key    string        `json:"key"`
	Fields jiraFields    `json:"fields"`
}

type jiraFields struct {
	Summary     string          `json:"summary"`
	Status      jiraStatus      `json:"status"`
	IssueType   jiraIssueType   `json:"issuetype"`
	Priority    jiraPriority    `json:"priority"`
	Assignee    *jiraUser       `json:"assignee"`
	Reporter    *jiraUser       `json:"reporter"`
	DueDate     *string         `json:"duedate"`
	Created     string          `json:"created"`
	Updated     string          `json:"updated"`
	Labels      []string        `json:"labels"`
	Description json.RawMessage `json:"description"`
}

func extractADFText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var node map[string]interface{}
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	return extractADFTextFromNode(node)
}

func extractADFTextFromNode(node map[string]interface{}) string {
	var parts []string
	if text, ok := node["text"].(string); ok && text != "" {
		parts = append(parts, text)
	}
	if content, ok := node["content"].([]interface{}); ok {
		for _, child := range content {
			if childMap, ok := child.(map[string]interface{}); ok {
				if t := extractADFTextFromNode(childMap); t != "" {
					parts = append(parts, t)
				}
			}
		}
	}
	return strings.Join(parts, " ")
}

type jiraStatus struct {
	Name string `json:"name"`
}

type jiraIssueType struct {
	Name    string `json:"name"`
	IconURL string `json:"iconUrl"`
}

type jiraPriority struct {
	Name string `json:"name"`
}

type jiraUser struct {
	DisplayName string `json:"displayName"`
}

// --- Config ---

type jiraConfig struct {
	Domain        string
	ShowAssigned  bool
	ShowWatching  bool
	ShowMentions  bool
	ShowSprint    bool
	JQL           string
	MaxIssues     int
}

// --- Plugin ---

type jiraPlugin struct {
	client        *http.Client
	config        jiraConfig
	domain        string
	cookieHeader  string
	prevIssueKeys map[string]string
}

func main() {
	logDebug("starting jira plugin")

	pl := &jiraPlugin{
		client:        &http.Client{Timeout: 15 * time.Second},
		config:        jiraConfig{ShowAssigned: true, MaxIssues: 10},
		prevIssueKeys: make(map[string]string),
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			logDebug("decode request failed: %v", err)
			continue
		}

		switch req.Method {
		case "initialize":
			if err := pl.handleInitialize(req); err != nil {
			logDebug("initialize failed: %v", err)
				sendError(req.ID, -32000, err.Error(), false, "Check Jira domain and session cookie.")
			}
		case "getStatus", "refresh":
			snap, err := pl.buildSnapshot()
			if err != nil {
				logDebug("buildSnapshot failed: %v", err)
				sendError(req.ID, -32000, err.Error(), true, "Jira API may be unreachable.")
				continue
			}
			sendResult(req.ID, snap)
		case "performAction":
			result, err := pl.handlePerformAction(req)
			if err != nil {
				logDebug("performAction failed: %v", err)
				sendError(req.ID, -32000, err.Error(), false, "Action failed.")
				continue
			}
			sendResult(req.ID, result)
		case "shutdown":
			logDebug("shutdown")
			sendResult(req.ID, nil)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		logDebug("scanner error: %v", err)
	}
}

func (p *jiraPlugin) handleInitialize(req rpcRequest) error {
	var params initializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fmt.Errorf("decode initialize: %w", err)
	}

	plugindebug.ConfigureFromInitializeConfig(params.Config)
	p.config = parseConfig(params.Config)
	p.domain = normalizeDomain(p.config.Domain)

	for _, pa := range params.ProviderAuths {
		if pa.Kind == "browser_import" && pa.CookieHeader != "" {
			p.cookieHeader = pa.CookieHeader
			break
		}
	}

	logDebug("initialize domain=%q cookie=%t assigned=%t watching=%t mentions=%t sprint=%t jql=%q max=%d",
		p.domain, p.cookieHeader != "", p.config.ShowAssigned, p.config.ShowWatching,
		p.config.ShowMentions, p.config.ShowSprint, p.config.JQL, p.config.MaxIssues)

	sendResult(req.ID, initializedPayload{
		Type:            "initialized",
		ProtocolVersion: protocolVersion,
		PluginVersion:   pluginVersion,
		Health:          "ok",
	})
	return nil
}

func (p *jiraPlugin) buildSnapshot() (*pluginSnapshot, error) {
	if p.domain == "" {
		return &pluginSnapshot{
			PluginID:     "jira",
			State:        "degraded",
			Summary:      pluginSummary{Title: "Jira", Value: "No domain", Severity: "info", IconHint: "jira"},
			RefreshAfter: 120,
			Health:       "degraded",
		}, nil
	}

	if p.cookieHeader == "" {
		return &pluginSnapshot{
			PluginID:     "jira",
			State:        "error",
			Summary:      pluginSummary{Title: "Jira", Value: "No session", Severity: "warning", IconHint: "jira"},
			Alerts:       []pluginAlert{{ID: "jira-no-auth", Severity: "warning", Message: "Import your browser session to connect to Jira."}},
			RefreshAfter: 120,
			Health:       "auth_required",
		}, nil
	}

	issues, err := p.fetchIssues()
	if err != nil {
		logDebug("fetchIssues error: %v", err)
		if apiErr, ok := err.(*apiHTTPError); ok {
			health := httphealth.ClassifyHTTPStatus(apiErr.StatusCode)
			msg := "Could not reach Jira: " + apiErr.Error()
			if health == httphealth.HealthAuthReq {
				msg = "Jira session expired — re-import your browser session in Settings"
			} else if health == httphealth.HealthRateLimited {
				msg = "Jira API rate limited"
			}
			return &pluginSnapshot{
				PluginID:     "jira",
				State:        "degraded",
				Summary:      pluginSummary{Title: "Jira", Value: "Err", Trend: "steady", Severity: "warning", IconHint: "jira"},
				Alerts:       []pluginAlert{{ID: "jira-fetch", Severity: "warning", Message: msg}},
				RefreshAfter: httphealth.DefaultRefreshAfter(health, apiErr.RetryAfter),
				Health:       health,
			}, nil
		}
		return &pluginSnapshot{
			PluginID:     "jira",
			State:        "degraded",
			Summary:      pluginSummary{Title: "Jira", Value: "Err", Trend: "steady", Severity: "warning", IconHint: "jira"},
			Alerts:       []pluginAlert{{ID: "jira-fetch", Severity: "warning", Message: "Could not reach Jira: " + err.Error()}},
			RefreshAfter: 120,
			Health:       "degraded",
		}, nil
	}

	items := p.buildItems(issues)
	alerts := p.buildAlerts(issues)
	summary := p.buildSummary(items, len(issues))

	prevKeys := p.prevIssueKeys
	currentKeys := make(map[string]string)
	for _, issue := range issues {
		currentKeys[issue.Key] = issue.Fields.Status.Name
	}

	for key, oldStatus := range prevKeys {
		newStatus, ok := currentKeys[key]
		if !ok {
			logDebug("issue resolved key=%s", key)
			emitEvent(pluginEvent{
				Type:     "issue.resolved",
				PluginID: "jira",
				Message:  fmt.Sprintf("%s was resolved or moved", key),
				Severity: "info",
				Data:     map[string]string{"issueKey": key, "url": fmt.Sprintf("%s/browse/%s", p.domain, key)},
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}
		if newStatus != oldStatus {
			logDebug("issue transition key=%s %s -> %s", key, oldStatus, newStatus)
			emitEvent(pluginEvent{
				Type:     "issue.updated",
				PluginID: "jira",
				Message:  fmt.Sprintf("%s moved to %s", key, newStatus),
				Severity: "info",
				Data:     map[string]string{"issueKey": key, "url": fmt.Sprintf("%s/browse/%s", p.domain, key)},
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	for key := range currentKeys {
		if _, ok := prevKeys[key]; !ok && len(prevKeys) > 0 {
			logDebug("new issue key=%s", key)
			emitEvent(pluginEvent{
				Type:     "issue.created",
				PluginID: "jira",
				Message:  fmt.Sprintf("New issue: %s", key),
				Severity: "info",
				Data:     map[string]string{"issueKey": key, "url": fmt.Sprintf("%s/browse/%s", p.domain, key)},
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	p.prevIssueKeys = currentKeys

	snap := &pluginSnapshot{
		PluginID:     "jira",
		State:        "ready",
		Summary:      summary,
		Items:        items,
		Actions:      []pluginAction{},
		Alerts:       alerts,
		RefreshAfter: 60,
		Health:       "ok",
	}
	return snap, nil
}

func (p *jiraPlugin) handlePerformAction(req rpcRequest) (*performActionResult, error) {
	if p.domain == "" {
		return nil, fmt.Errorf("no Jira domain configured")
	}
	if p.cookieHeader == "" {
		return nil, fmt.Errorf("no Jira session configured")
	}

	var params performActionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("decode performAction params: %w", err)
	}

	switch params.ActionID {
	case "getIssueDetails":
		key := strings.TrimSpace(params.Payload["key"])
		if key == "" {
			return nil, fmt.Errorf("missing key payload")
		}
		return p.getIssueDetails(key)
	case "searchIssues":
		jql := strings.TrimSpace(params.Payload["jql"])
		if jql == "" {
			return nil, fmt.Errorf("missing jql payload")
		}
		return p.searchIssues(jql)
	default:
		return nil, fmt.Errorf("unknown action %q", params.ActionID)
	}
}

func (p *jiraPlugin) getIssueDetails(key string) (*performActionResult, error) {
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,status,issuetype,priority,assignee,reporter,duedate,created,updated,labels,description,comment", p.domain, key)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", p.cookieHeader)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var issue jiraIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, err
	}

	description := strings.TrimSpace(extractADFText(issue.Fields.Description))
	if description == "" {
		description = "(no description)"
	}

	var comments []string
	// comment field is not in our jiraFields struct; decode from raw body
	var rawIssue struct {
		Fields struct {
			Comment struct {
				Comments []struct {
					Author  jiraUser `json:"author"`
					Body    json.RawMessage `json:"body"`
					Created string `json:"created"`
				} `json:"comments"`
			} `json:"comment"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &rawIssue); err == nil {
		for _, c := range rawIssue.Fields.Comment.Comments {
			text := strings.TrimSpace(extractADFText(c.Body))
			if text != "" {
				comments = append(comments, fmt.Sprintf("%s (%s): %s", c.Author.DisplayName, humanDate(c.Created), text))
			}
		}
	}

	assignee := "Unassigned"
	if issue.Fields.Assignee != nil {
		assignee = issue.Fields.Assignee.DisplayName
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Issue: %s %s", issue.Key, issue.Fields.Summary))
	lines = append(lines, fmt.Sprintf("Status: %s | Type: %s | Priority: %s | Assignee: %s", issue.Fields.Status.Name, issue.Fields.IssueType.Name, issue.Fields.Priority.Name, assignee))
	lines = append(lines, fmt.Sprintf("Description: %s", description))
	if len(comments) > 0 {
		lines = append(lines, "Comments:")
		for _, c := range comments {
			lines = append(lines, "  - "+c)
		}
	}
	lines = append(lines, fmt.Sprintf("Link: %s/browse/%s", p.domain, issue.Key))

	return &performActionResult{Success: true, Data: strings.Join(lines, "\n")}, nil
}

func (p *jiraPlugin) searchIssues(jql string) (*performActionResult, error) {
	issues, err := p.fetchIssuesWithJQL(jql)
	if err != nil {
		return nil, err
	}

	if len(issues) == 0 {
		return &performActionResult{Success: true, Data: "No issues found."}, nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d issue(s):", len(issues)))
	for _, issue := range issues {
		description := strings.TrimSpace(extractADFText(issue.Fields.Description))
		line := fmt.Sprintf("%s %s | %s | %s", issue.Key, issue.Fields.Summary, issue.Fields.Status.Name, issue.Fields.IssueType.Name)
		if description != "" {
			line += " | " + truncate(description, 120)
		}
		lines = append(lines, "  - "+line)
	}

	return &performActionResult{Success: true, Data: strings.Join(lines, "\n")}, nil
}

func (p *jiraPlugin) fetchIssuesWithJQL(jql string) ([]jiraIssue, error) {
	apiURL := fmt.Sprintf("%s/rest/api/3/search/jql", p.domain)

	reqBody := struct {
		JQL        string   `json:"jql"`
		MaxResults int      `json:"maxResults"`
		Fields     []string `json:"fields"`
	}{
		JQL:        jql,
		MaxResults: p.config.MaxIssues,
		Fields:     []string{"summary", "status", "issuetype", "priority", "assignee", "reporter", "duedate", "created", "updated", "labels", "description"},
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", p.cookieHeader)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result jiraSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result.Issues, nil
}

func humanDate(iso string) string {
	t, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return iso
	}
	return t.Format("Jan 2")
}

func (p *jiraPlugin) fetchIssues() ([]jiraIssue, error) {
	jql := p.buildJQL()
	apiURL := fmt.Sprintf("%s/rest/api/3/search/jql", p.domain)

	logDebug("fetching url=%s jql=%s max=%d", apiURL, jql, p.config.MaxIssues)

	reqBody := struct {
		JQL        string   `json:"jql"`
		MaxResults int      `json:"maxResults"`
		Fields     []string `json:"fields"`
	}{
		JQL:        jql,
		MaxResults: p.config.MaxIssues,
		Fields:     []string{"summary", "status", "issuetype", "priority", "assignee", "reporter", "duedate", "created", "updated", "labels", "description"},
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", p.cookieHeader)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, &apiHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       string(body),
		}
	}

	var result jiraSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	logDebug("got total=%d issues=%d", result.Total, len(result.Issues))
	return result.Issues, nil
}

func (p *jiraPlugin) buildJQL() string {
	if strings.TrimSpace(p.config.JQL) != "" {
		jql := strings.TrimSpace(p.config.JQL)
		return fmt.Sprintf("%s ORDER BY updated DESC", jql)
	}

	var parts []string

	if p.config.ShowAssigned {
		parts = append(parts, "assignee = currentUser()")
	}

	if p.config.ShowWatching {
		parts = append(parts, "watcher = currentUser()")
	}

	if p.config.ShowMentions {
		q := fmt.Sprintf("(text ~ currentUser() OR comment ~ currentUser())")
		parts = append(parts, q)
	}

	if p.config.ShowSprint {
		parts = append(parts, "sprint IN openSprints()")
	}

	if len(parts) == 0 {
		parts = append(parts, "assignee = currentUser()")
	}

	clause := strings.Join(parts, " OR ")
	return fmt.Sprintf("(%s) AND statusCategory != Done ORDER BY updated DESC", clause)
}

func (p *jiraPlugin) buildItems(issues []jiraIssue) []pluginItem {
	items := make([]pluginItem, 0, len(issues))
	for _, issue := range issues {
		detail := fmt.Sprintf("%s | %s", issue.Fields.IssueType.Name, issue.Fields.Status.Name)
		if issue.Fields.Priority.Name != "" {
			detail = fmt.Sprintf("%s | %s", issue.Fields.Priority.Name, detail)
		}

		description := strings.TrimSpace(extractADFText(issue.Fields.Description))
		if description != "" {
			detail = fmt.Sprintf("%s | %s", truncate(description, 160), detail)
		}

		subtitle := "Unassigned"
		if issue.Fields.Assignee != nil {
			subtitle = issue.Fields.Assignee.DisplayName
		}

		severity := "info"
		if issue.Fields.Priority.Name == "Highest" || issue.Fields.Priority.Name == "High" {
			severity = "warning"
		}

		if issue.Fields.DueDate != nil {
			due, err := time.Parse("2006-01-02", *issue.Fields.DueDate)
			if err == nil && due.Before(time.Now()) {
				severity = "critical"
				detail = fmt.Sprintf("OVERDUE | %s", detail)
			}
		}

		deepLink := fmt.Sprintf("%s/browse/%s", p.domain, issue.Key)

		items = append(items, pluginItem{
			ID:        issue.Key,
			Title:     fmt.Sprintf("%s %s", issue.Key, issue.Fields.Summary),
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  severity,
			Timestamp: issue.Fields.Updated,
			DeepLink:  deepLink,
			Actions:   []pluginAction{},
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Timestamp > items[j].Timestamp
	})

	return items
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func (p *jiraPlugin) buildAlerts(issues []jiraIssue) []pluginAlert {
	alerts := []pluginAlert{}

	overdueCount := 0
	for _, issue := range issues {
		if issue.Fields.DueDate != nil {
			due, err := time.Parse("2006-01-02", *issue.Fields.DueDate)
			if err == nil && due.Before(time.Now()) {
				overdueCount++
			}
		}
	}

	if overdueCount > 0 {
		alerts = append(alerts, pluginAlert{
			ID:       "jira-overdue",
			Severity: "critical",
			Message:  fmt.Sprintf("%d overdue issue(s)", overdueCount),
		})
	}

	return alerts
}

func (p *jiraPlugin) buildSummary(items []pluginItem, total int) pluginSummary {
	if total == 0 {
		return pluginSummary{
			Title:    "Jira",
			Value:    "0 issues",
			Trend:    "steady",
			Severity: "info",
			IconHint: "jira",
		}
	}

	value := fmt.Sprintf("%d", len(items))
	severity := "info"
	for _, item := range items {
		if item.Severity == "critical" {
			severity = "critical"
			break
		}
		if item.Severity == "warning" && severity != "critical" {
			severity = "warning"
		}
	}

	if total > p.config.MaxIssues {
		value = fmt.Sprintf("%d+", p.config.MaxIssues)
	}

	return pluginSummary{
		Title:    "Jira",
		Value:    fmt.Sprintf("%s issues", value),
		Trend:    "steady",
		Severity: severity,
		IconHint: "jira",
	}
}

// --- Helpers ---

func parseConfig(cfg map[string]string) jiraConfig {
	config := jiraConfig{
		ShowAssigned: true,
		MaxIssues:    10,
	}
	if v, ok := cfg["domain"]; ok {
		config.Domain = strings.TrimSpace(v)
	}
	if v, ok := cfg["showAssigned"]; ok {
		config.ShowAssigned = parseBool(v, true)
	}
	if v, ok := cfg["showWatching"]; ok {
		config.ShowWatching = parseBool(v, false)
	}
	if v, ok := cfg["showMentions"]; ok {
		config.ShowMentions = parseBool(v, false)
	}
	if v, ok := cfg["showSprint"]; ok {
		config.ShowSprint = parseBool(v, false)
	}
	if v, ok := cfg["jql"]; ok {
		config.JQL = strings.TrimSpace(v)
	}
	if v, ok := cfg["maxIssues"]; ok {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			config.MaxIssues = n
		}
	}
	return config
}

func normalizeDomain(raw string) string {
	d := strings.TrimSpace(raw)
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "https://") && !strings.HasPrefix(d, "http://") {
		d = "https://" + d
	}
	u, err := url.Parse(d)
	if err != nil {
		return strings.TrimRight(d, "/")
	}
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

func parseBool(raw string, defaultValue bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return defaultValue
	}
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

func emitEvent(event pluginEvent) {
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
