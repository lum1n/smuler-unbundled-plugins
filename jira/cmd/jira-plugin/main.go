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
	pluginVersion = "0.1.2"
)

// apiHTTPError carries only the status; response bodies are never surfaced
// (they can be large HTML login pages or echo request details).
type apiHTTPError struct {
	StatusCode int
	RetryAfter int
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d", e.StatusCode)
}

const maxBodyBytes = 4 << 20

// searchFields are the issue fields requested for list/search views.
var searchFields = []string{"summary", "status", "issuetype", "priority", "assignee", "duedate", "updated", "description"}

// --- Jira API types ---

// jiraSearchResponse is the /rest/api/3/search/jql shape. That endpoint does
// not return "total"; more results are signalled by isLast/nextPageToken.
type jiraSearchResponse struct {
	Issues        []jiraIssue `json:"issues"`
	IsLast        *bool       `json:"isLast"`
	NextPageToken string      `json:"nextPageToken"`
}

func (r jiraSearchResponse) hasMore() bool {
	if r.IsLast != nil {
		return !*r.IsLast
	}
	return r.NextPageToken != ""
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
	prevOverdue   int
	seeded        bool // true once a successful snapshot populated prevIssueKeys
}

func (p *jiraHandler) Initialize(params sdk.InitializeParams) string {
	p.config = parseConfig(params.Config)
	p.domain = normalizeDomain(p.config.Domain)

	// The host fills cookieHeader for both browser_import and web_session records.
	for _, pa := range params.ProviderAuths {
		if c := strings.TrimSpace(pa.CookieHeader); c != "" {
			p.cookieHeader = c
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

	issues, hasMore, err := p.fetchIssues()
	if err != nil {
		sdk.Log("fetchIssues error: %v", err)
		if apiErr, ok := err.(*apiHTTPError); ok {
			health := httphealth.ClassifyHTTPStatus(apiErr.StatusCode)
			msg := "Jira API error (" + apiErr.Error() + ")"
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
			Alerts:       []sdk.Alert{{ID: "jira-fetch", Severity: sdk.SeverityWarning, Message: "Could not reach Jira"}},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	items := p.buildItems(issues)
	alerts := p.buildAlerts(issues)
	summary := p.buildSummary(items, hasMore)

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
	overdue := 0
	for _, issue := range issues {
		currentKeys[issue.Key] = issue.Fields.Status.Name
		if isOverdue(issue, time.Now()) {
			overdue++
		}
	}

	if !p.seeded {
		p.prevIssueKeys = currentKeys
		p.prevOverdue = overdue
		p.seeded = true
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	if overdue > p.prevOverdue {
		sdk.Emit(sdk.Event{
			Type:      "issue.overdue",
			PluginID:  pluginID,
			Message:   fmt.Sprintf("%d overdue issue(s) need attention", overdue),
			Severity:  sdk.SeverityWarning,
			Data:      map[string]string{"count": strconv.Itoa(overdue)},
			Timestamp: now,
		})
	}
	p.prevOverdue = overdue

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
		if _, ok := prevKeys[key]; !ok {
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
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,status,issuetype,priority,assignee,reporter,duedate,created,updated,labels,description,comment", p.domain, url.PathEscape(key))

	body, err := p.doJSON("GET", apiURL, nil)
	if err != nil {
		return false, userFacingError(err)
	}

	var issue jiraIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return false, "could not decode Jira response"
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

// doJSON performs an authenticated Jira REST call using the imported browser
// session and returns the body of a 2xx JSON response.
func (p *jiraHandler) doJSON(method, apiURL string, payload []byte) ([]byte, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, apiURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", p.cookieHeader)
	// Cookie-authenticated writes (including POST search) must opt out of
	// Jira's XSRF check, otherwise Jira answers 403.
	req.Header.Set("X-Atlassian-Token", "no-check")
	req.Header.Set("User-Agent", "smuler-jira-plugin/"+pluginVersion)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	// An expired session is often redirected to the HTML login page (200).
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "text/html") {
		return nil, &apiHTTPError{StatusCode: http.StatusUnauthorized}
	}
	return body, nil
}

func userFacingError(err error) string {
	if apiErr, ok := err.(*apiHTTPError); ok {
		switch httphealth.ClassifyHTTPStatus(apiErr.StatusCode) {
		case httphealth.HealthAuthReq:
			return "Jira session expired — re-import your browser session in Settings"
		case httphealth.HealthRateLimited:
			return "Jira API rate limited"
		}
		return "Jira API error (" + apiErr.Error() + ")"
	}
	return "Could not reach Jira"
}

func (p *jiraHandler) searchIssues(jql string) (bool, string) {
	issues, _, err := p.fetchIssuesWithJQL(jql)
	if err != nil {
		return false, userFacingError(err)
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

func (p *jiraHandler) fetchIssuesWithJQL(jql string) ([]jiraIssue, bool, error) {
	apiURL := fmt.Sprintf("%s/rest/api/3/search/jql", p.domain)

	reqBody := struct {
		JQL        string   `json:"jql"`
		MaxResults int      `json:"maxResults"`
		Fields     []string `json:"fields"`
	}{
		JQL:        jql,
		MaxResults: p.config.MaxIssues,
		Fields:     searchFields,
	}
	bodyJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}

	body, err := p.doJSON("POST", apiURL, bodyJSON)
	if err != nil {
		return nil, false, err
	}

	var result jiraSearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("decode response: %w", err)
	}
	return result.Issues, result.hasMore(), nil
}

func humanDate(iso string) string {
	t, ok := parseJiraTime(iso)
	if !ok {
		return iso
	}
	return t.Format("Jan 2")
}

// parseJiraTime accepts Jira's "2006-01-02T15:04:05.000-0700" as well as RFC3339.
func parseJiraTime(raw string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// isoTimestamp converts a Jira timestamp to RFC3339 UTC, the format the host parses.
func isoTimestamp(raw string) string {
	t, ok := parseJiraTime(raw)
	if !ok {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// isOverdue reports whether the due date is before today (local time). A due
// date of today is not yet overdue.
func isOverdue(issue jiraIssue, now time.Time) bool {
	if issue.Fields.DueDate == nil {
		return false
	}
	due, err := time.ParseInLocation("2006-01-02", *issue.Fields.DueDate, now.Location())
	if err != nil {
		return false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return due.Before(today)
}

func (p *jiraHandler) fetchIssues() ([]jiraIssue, bool, error) {
	jql := p.buildJQL()
	sdk.Log("fetching domain=%s jql=%s max=%d", p.domain, jql, p.config.MaxIssues)

	issues, hasMore, err := p.fetchIssuesWithJQL(jql)
	if err != nil {
		return nil, false, err
	}
	sdk.Log("got issues=%d more=%t", len(issues), hasMore)
	return issues, hasMore, nil
}

func (p *jiraHandler) buildJQL() string {
	if strings.TrimSpace(p.config.JQL) != "" {
		jql := strings.TrimSpace(p.config.JQL)
		if strings.Contains(strings.ToLower(jql), "order by") {
			return jql
		}
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

		if isOverdue(issue, time.Now()) {
			severity = sdk.SeverityCritical
			detail = fmt.Sprintf("OVERDUE | %s", detail)
		}

		deepLink := fmt.Sprintf("%s/browse/%s", p.domain, issue.Key)

		items = append(items, sdk.Item{
			ID:        issue.Key,
			Title:     fmt.Sprintf("%s %s", issue.Key, issue.Fields.Summary),
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  severity,
			Timestamp: isoTimestamp(issue.Fields.Updated),
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
		if isOverdue(issue, time.Now()) {
			overdueCount++
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

func (p *jiraHandler) buildSummary(items []sdk.Item, hasMore bool) sdk.Summary {
	if len(items) == 0 {
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

	if hasMore {
		value = fmt.Sprintf("%d+", len(items))
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
