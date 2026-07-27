package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "jira"
	pluginVersion   = "0.1.2"
)

type apiHTTPError struct {
	StatusCode int
	RetryAfter int
	Body       string
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// --- Jira API types ---

type jiraSearchResponse struct {
	Issues []jiraIssue `json:"issues"`
	Total  int         `json:"total"`
}

type jiraIssue struct {
	ID     string     `json:"id"`
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
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
	Domain       string
	ShowAssigned bool
	ShowWatching bool
	ShowMentions bool
	ShowSprint   bool
	JQL          string
	MaxIssues    int
}

// --- Plugin ---

type jiraHandler struct {
	client        *http.Client
	config        jiraConfig
	domain        string
	cookieHeader  string
	prevIssueKeys map[string]string
}

func (p *jiraHandler) Initialize(params sdk.InitializeParams) string {
	p.config = parseConfig(params.Config)
	p.domain = normalizeDomain(p.config.Domain)

	for _, pa := range params.ProviderAuths {
		if pa.Kind == "browser_import" && pa.CookieHeader != "" {
			p.cookieHeader = pa.CookieHeader
			break
		}
	}

	sdk.Log("initialize domain=%q cookie=%t assigned=%t watching=%t mentions=%t sprint=%t jql=%q max=%d",
		p.domain, p.cookieHeader != "", p.config.ShowAssigned, p.config.ShowWatching,
		p.config.ShowMentions, p.config.ShowSprint, p.config.JQL, p.config.MaxIssues)

	if p.domain == "" || p.cookieHeader == "" {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

func (p *jiraHandler) GetStatus() sdk.Snapshot {
	if p.domain == "" {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateDegraded,
			Summary:      sdk.Summary{Title: "Jira", Value: "No domain", Severity: sdk.SeverityInfo, IconHint: "jira"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	if p.cookieHeader == "" {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateError,
			Summary:      sdk.Summary{Title: "Jira", Value: "No session", Severity: sdk.SeverityWarning, IconHint: "jira"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "jira-no-auth", Severity: sdk.SeverityWarning, Message: "Import your browser session to connect to Jira."}},
			RefreshAfter: 120,
			Health:       sdk.HealthAuthReq,
		}
	}

	issues, err := p.fetchIssues()
	if err != nil {
		sdk.Log("fetchIssues error: %v", err)
		if apiErr, ok := err.(*apiHTTPError); ok {
			health := httphealth.ClassifyHTTPStatus(apiErr.StatusCode)
			msg := "Could not reach Jira: " + apiErr.Error()
			if health == httphealth.HealthAuthReq {
				msg = "Jira session expired — re-import your browser session in Settings"
			} else if health == httphealth.HealthRateLimited {
				msg = "Jira API rate limited"
			}
			return sdk.Snapshot{
				PluginID:     pluginID,
				State:        sdk.StateDegraded,
				Summary:      sdk.Summary{Title: "Jira", Value: "Err", Trend: sdk.TrendSteady, Severity: sdk.SeverityWarning, IconHint: "jira"},
				Items:        []sdk.Item{},
				Actions:      []sdk.Action{},
				Alerts:       []sdk.Alert{{ID: "jira-fetch", Severity: sdk.SeverityWarning, Message: msg}},
				RefreshAfter: httphealth.DefaultRefreshAfter(health, apiErr.RetryAfter),
				Health:       health,
			}
		}
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateDegraded,
			Summary:      sdk.Summary{Title: "Jira", Value: "Err", Trend: sdk.TrendSteady, Severity: sdk.SeverityWarning, IconHint: "jira"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "jira-fetch", Severity: sdk.SeverityWarning, Message: "Could not reach Jira: " + err.Error()}},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	items := p.buildItems(issues)
	alerts := p.buildAlerts(issues)
	summary := p.buildSummary(items, len(issues))

	p.emitDeltas(issues)

	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateReady,
		Summary:      summary,
		Items:        items,
		Actions:      []sdk.Action{},
		Alerts:       alerts,
		RefreshAfter: 60,
		Health:       sdk.HealthOK,
	}
}

func (p *jiraHandler) emitDeltas(issues []jiraIssue) {
	prevKeys := p.prevIssueKeys
	currentKeys := make(map[string]string)
	for _, issue := range issues {
		currentKeys[issue.Key] = issue.Fields.Status.Name
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for key, oldStatus := range prevKeys {
		newStatus, ok := currentKeys[key]
		if !ok {
			sdk.Log("issue resolved key=%s", key)
			sdk.Emit(sdk.Event{
				Type:      "issue.resolved",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s was resolved or moved", key),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"issueKey": key, "url": fmt.Sprintf("%s/browse/%s", p.domain, key)},
				Timestamp: now,
			})
			continue
		}
		if newStatus != oldStatus {
			sdk.Log("issue transition key=%s %s -> %s", key, oldStatus, newStatus)
			sdk.Emit(sdk.Event{
				Type:      "issue.updated",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s moved to %s", key, newStatus),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"issueKey": key, "url": fmt.Sprintf("%s/browse/%s", p.domain, key)},
				Timestamp: now,
			})
		}
	}

	for key := range currentKeys {
		if _, ok := prevKeys[key]; !ok && len(prevKeys) > 0 {
			sdk.Log("new issue key=%s", key)
			sdk.Emit(sdk.Event{
				Type:      "issue.created",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("New issue: %s", key),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"issueKey": key, "url": fmt.Sprintf("%s/browse/%s", p.domain, key)},
				Timestamp: now,
			})
		}
	}

	p.prevIssueKeys = currentKeys
}

func (p *jiraHandler) PerformAction(id string, params map[string]string) (bool, string) {
	if p.domain == "" {
		return false, "no Jira domain configured"
	}
	if p.cookieHeader == "" {
		return false, "no Jira session configured"
	}

	switch id {
	case "getIssueDetails":
		key := strings.TrimSpace(params["key"])
		if key == "" {
			return false, "missing key payload"
		}
		return p.getIssueDetails(key)
	case "searchIssues":
		jql := strings.TrimSpace(params["jql"])
		if jql == "" {
			return false, "missing jql payload"
		}
		return p.searchIssues(jql)
	default:
		return false, fmt.Sprintf("unknown action %q", id)
	}
}

func (p *jiraHandler) Shutdown() {}

func (p *jiraHandler) getIssueDetails(key string) (bool, string) {
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,status,issuetype,priority,assignee,reporter,duedate,created,updated,labels,description,comment", p.domain, key)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", p.cookieHeader)

	resp, err := p.client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err.Error()
	}
	if resp.StatusCode != 200 {
		return false, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var issue jiraIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return false, err.Error()
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
					Author  jiraUser        `json:"author"`
					Body    json.RawMessage `json:"body"`
					Created string          `json:"created"`
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

	return true, strings.Join(lines, "\n")
}

func (p *jiraHandler) searchIssues(jql string) (bool, string) {
	issues, err := p.fetchIssuesWithJQL(jql)
	if err != nil {
		return false, err.Error()
	}

	if len(issues) == 0 {
		return true, "No issues found."
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

	return true, strings.Join(lines, "\n")
}

func (p *jiraHandler) fetchIssuesWithJQL(jql string) ([]jiraIssue, error) {
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

func (p *jiraHandler) fetchIssues() ([]jiraIssue, error) {
	jql := p.buildJQL()
	apiURL := fmt.Sprintf("%s/rest/api/3/search/jql", p.domain)

	sdk.Log("fetching url=%s jql=%s max=%d", apiURL, jql, p.config.MaxIssues)

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

	sdk.Log("got total=%d issues=%d", result.Total, len(result.Issues))
	return result.Issues, nil
}

func (p *jiraHandler) buildJQL() string {
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

func (p *jiraHandler) buildItems(issues []jiraIssue) []sdk.Item {
	items := make([]sdk.Item, 0, len(issues))
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

		severity := sdk.SeverityInfo
		if issue.Fields.Priority.Name == "Highest" || issue.Fields.Priority.Name == "High" {
			severity = sdk.SeverityWarning
		}

		if issue.Fields.DueDate != nil {
			due, err := time.Parse("2006-01-02", *issue.Fields.DueDate)
			if err == nil && due.Before(time.Now()) {
				severity = sdk.SeverityCritical
				detail = fmt.Sprintf("OVERDUE | %s", detail)
			}
		}

		deepLink := fmt.Sprintf("%s/browse/%s", p.domain, issue.Key)

		items = append(items, sdk.Item{
			ID:        issue.Key,
			Title:     fmt.Sprintf("%s %s", issue.Key, issue.Fields.Summary),
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  severity,
			Timestamp: issue.Fields.Updated,
			DeepLink:  deepLink,
			Actions:   []sdk.Action{},
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

func (p *jiraHandler) buildAlerts(issues []jiraIssue) []sdk.Alert {
	alerts := []sdk.Alert{}

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
		alerts = append(alerts, sdk.Alert{
			ID:       "jira-overdue",
			Severity: sdk.SeverityCritical,
			Message:  fmt.Sprintf("%d overdue issue(s)", overdueCount),
		})
	}

	return alerts
}

func (p *jiraHandler) buildSummary(items []sdk.Item, total int) sdk.Summary {
	if total == 0 {
		return sdk.Summary{
			Title:    "Jira",
			Value:    "0 issues",
			Trend:    sdk.TrendSteady,
			Severity: sdk.SeverityInfo,
			IconHint: "jira",
		}
	}

	value := fmt.Sprintf("%d", len(items))
	severity := sdk.SeverityInfo
	for _, item := range items {
		if item.Severity == sdk.SeverityCritical {
			severity = sdk.SeverityCritical
			break
		}
		if item.Severity == sdk.SeverityWarning && severity != sdk.SeverityCritical {
			severity = sdk.SeverityWarning
		}
	}

	if total > p.config.MaxIssues {
		value = fmt.Sprintf("%d+", p.config.MaxIssues)
	}

	return sdk.Summary{
		Title:    "Jira",
		Value:    fmt.Sprintf("%s issues", value),
		Trend:    sdk.TrendSteady,
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

func main() {
	sdk.Run(pluginID, pluginVersion, &jiraHandler{
		client:        &http.Client{Timeout: 15 * time.Second},
		config:        jiraConfig{ShowAssigned: true, MaxIssues: 10},
		prevIssueKeys: make(map[string]string),
	})
}
