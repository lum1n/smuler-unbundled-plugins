package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	apiURL        = "https://api.linear.app/graphql"
	pluginID      = "linear"
	pluginVersion = "0.1.1"
)

type linearConfig struct {
	ShowAssigned bool
	ShowMentions bool
	ShowTriage   bool
	TeamIDs      []string
}

type linearHandler struct {
	token          string
	config         linearConfig
	client         *http.Client
	lastError      string
	lastRetryAfter int
	lastHTTPStatus int

	// State tracking for delta detection
	prevIssueIDs     map[string]prevIssueInfo // id -> previous state
	prevOverdueCount int
	seeded           bool // true once a successful snapshot populated prevIssueIDs

	apiURL string // overridable for tests
}

type prevIssueInfo struct {
	kind       string
	url        string
	identifier string
}

type graphqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type graphqlResponse struct {
	Data   graphqlData    `json:"data"`
	Errors []graphqlError `json:"errors"`
}

type graphqlError struct {
	Message    string `json:"message"`
	Type       string `json:"type"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

type graphqlData struct {
	Viewer        viewerData             `json:"viewer"`
	Notifications notificationConnection `json:"notifications"`
	Triage        issueConnection        `json:"triageIssues"`
}

type viewerData struct {
	Assigned issueConnection `json:"assignedIssues"`
}

type issueConnection struct {
	Nodes []issueNode `json:"nodes"`
}

type notificationConnection struct {
	Nodes []notificationNode `json:"nodes"`
}

type issueNode struct {
	ID         string     `json:"id"`
	Identifier string     `json:"identifier"`
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	UpdatedAt  string     `json:"updatedAt"`
	DueDate    string     `json:"dueDate"`
	Priority   int        `json:"priority"`
	State      issueState `json:"state"`
	Team       issueTeam  `json:"team"`
}

type issueState struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type issueTeam struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	ID   string `json:"id"`
}

type notificationNode struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	UpdatedAt string     `json:"updatedAt"`
	ReadAt    *string    `json:"readAt"`
	Issue     *issueNode `json:"issue"`
}

type signal struct {
	kind      string
	issue     issueNode
	updatedAt time.Time
	severity  string
	detail    string
	alert     string
	uniqueID  string
}

func (p *linearHandler) Initialize(params sdk.InitializeParams) string {
	p.token = resolveAuthHeader(params)
	p.config = parseConfig(params.Config)
	sdk.Log("initialize token=%t assigned=%t mentions=%t triage=%t teamIds=%q", p.token != "", p.config.ShowAssigned, p.config.ShowMentions, p.config.ShowTriage, strings.Join(p.config.TeamIDs, ","))
	return sdk.HealthOK
}

// resolveAuthHeader returns the Authorization header value. Linear personal API
// keys are sent bare, while OAuth access tokens require the "Bearer " prefix.
// Provider auth records carry the secret in accessToken/apiKey (accountId is a
// record id); the legacy auth context carries the raw token in accountId.
func resolveAuthHeader(params sdk.InitializeParams) string {
	for _, auth := range params.ProviderAuths {
		if t := strings.TrimSpace(auth.AccessToken); t != "" {
			return authHeaderValue(t, true)
		}
		if t := strings.TrimSpace(auth.APIKey); t != "" {
			return authHeaderValue(t, false)
		}
	}
	if params.Auth != nil {
		if t := strings.TrimSpace(params.Auth.AccountID); t != "" {
			return authHeaderValue(t, false)
		}
	}
	return ""
}

func authHeaderValue(token string, oauth bool) string {
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	if strings.HasPrefix(token, "lin_api_") {
		return token
	}
	if oauth || strings.HasPrefix(token, "lin_oauth_") {
		return "Bearer " + token
	}
	return token
}

func parseConfig(cfg map[string]string) linearConfig {
	config := linearConfig{ShowAssigned: true}
	if v, ok := cfg["showAssigned"]; ok {
		config.ShowAssigned = parseBool(v, true)
	}
	if v, ok := cfg["showMentions"]; ok {
		config.ShowMentions = parseBool(v, false)
	}
	if v, ok := cfg["showTriage"]; ok {
		config.ShowTriage = parseBool(v, false)
	}
	if v, ok := cfg["teamIds"]; ok {
		config.TeamIDs = splitCSV(v)
	}
	return config
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

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func (p *linearHandler) GetStatus() sdk.Snapshot {
	return p.buildSnapshot()
}

func (p *linearHandler) PerformAction(id string, params map[string]string) (bool, string) {
	return false, "unknown action: " + id
}

func (p *linearHandler) Shutdown() {}

func (p *linearHandler) buildSnapshot() sdk.Snapshot {
	if strings.TrimSpace(p.token) == "" {
		return emptySnapshot("No token configured", sdk.HealthAuthReq, 0)
	}

	p.lastError = ""
	p.lastRetryAfter = 0
	p.lastHTTPStatus = 0
	data, rateLimited, err := p.fetchData()
	if err != nil {
		message := p.lastError
		if message == "" {
			message = "Linear API unreachable"
		}
		if rateLimited {
			return emptySnapshot(message, sdk.HealthRateLimited, p.lastRetryAfter)
		}
		if p.lastHTTPStatus == http.StatusUnauthorized || p.lastHTTPStatus == http.StatusForbidden {
			return emptySnapshot("Linear authentication failed — reconnect in Settings", sdk.HealthAuthReq, p.lastRetryAfter)
		}
		return emptySnapshot(message, sdk.HealthDegraded, p.lastRetryAfter)
	}

	signals := collectSignals(data, p.config)

	// Emit events for state changes
	p.emitDeltaEvents(signals)

	if len(signals) == 0 {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateReady,
			Summary:      sdk.Summary{Title: "Linear", Value: "No active issues", Trend: sdk.TrendSteady, Severity: sdk.SeverityInfo, IconHint: "circle.grid.2x2"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
			Alerts:       []sdk.Alert{},
			RefreshAfter: 300,
			Health:       sdk.HealthOK,
		}
	}

	items := make([]sdk.Item, 0, len(signals))
	alerts := make([]sdk.Alert, 0)
	alertIDs := map[string]struct{}{}
	overdueCount := 0
	mentionCount := 0
	triageCount := 0

	for _, signal := range signals {
		items = append(items, toItem(signal))
		if signal.alert != "" {
			if _, ok := alertIDs[signal.alert]; !ok {
				alertIDs[signal.alert] = struct{}{}
				alerts = append(alerts, sdk.Alert{ID: signal.uniqueID + "-alert", Severity: signal.severity, Message: signal.alert})
			}
		}
		if strings.Contains(strings.ToLower(signal.detail), "overdue") {
			overdueCount++
		}
		switch signal.kind {
		case "mention":
			mentionCount++
		case "triage":
			triageCount++
		}
	}

	severity := sdk.SeverityInfo
	value := fmt.Sprintf("%d assigned", countKind(signals, "assigned"))
	trend := sdk.TrendSteady
	if overdueCount > 0 {
		severity = sdk.SeverityWarning
		value = fmt.Sprintf("%d overdue", overdueCount)
	}
	if overdueCount == 0 && mentionCount > 0 {
		value = fmt.Sprintf("%d mentions", mentionCount)
		trend = sdk.TrendUp
	}
	if overdueCount == 0 && mentionCount == 0 && triageCount > 0 {
		value = fmt.Sprintf("%d triage", triageCount)
	}

	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateReady,
		Summary:      sdk.Summary{Title: "Linear", Value: value, Trend: trend, Severity: severity, IconHint: "circle.grid.2x2"},
		Items:        items,
		Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: 300,
		Health:       sdk.HealthOK,
	}
}

func countKind(signals []signal, kind string) int {
	count := 0
	for _, signal := range signals {
		if signal.kind == kind {
			count++
		}
	}
	return count
}

func emptySnapshot(message, health string, retryAfter int) sdk.Snapshot {
	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateDegraded,
		Summary:      sdk.Summary{Title: "Linear", Value: message, Trend: sdk.TrendSteady, Severity: sdk.SeverityInfo, IconHint: "circle.grid.2x2"},
		Items:        []sdk.Item{},
		Actions:      []sdk.Action{},
		Alerts:       []sdk.Alert{{ID: "linear-config", Severity: sdk.SeverityInfo, Message: message}},
		RefreshAfter: httphealth.DefaultRefreshAfter(health, retryAfter),
		Health:       health,
	}
}

func (p *linearHandler) emitDeltaEvents(signals []signal) {
	currentIDs := make(map[string]prevIssueInfo) // id -> info
	currentOverdue := 0

	for _, s := range signals {
		currentIDs[s.uniqueID] = prevIssueInfo{kind: s.kind, url: s.issue.URL, identifier: s.issue.Identifier}
		if strings.Contains(strings.ToLower(s.detail), "overdue") {
			currentOverdue++
		}
	}

	if p.seeded {
		// Detect new issues
		for id, info := range currentIDs {
			if _, existed := p.prevIssueIDs[id]; !existed {
				displayID := info.identifier
				if displayID == "" {
					displayID = id
				}
				label := info.kind
				switch info.kind {
				case "assigned":
					label = fmt.Sprintf("New assigned issue: %s", displayID)
				case "mention":
					label = fmt.Sprintf("New mention: %s", displayID)
				case "triage":
					label = fmt.Sprintf("New triage issue: %s", displayID)
				}
				sdk.Emit(sdk.Event{
					Type:     "issue.created",
					PluginID: pluginID,
					Message:  label,
					Severity: sdk.SeverityInfo,
					Data:     map[string]string{"issueId": id, "kind": info.kind, "url": info.url},
				})
			}
		}

		// Detect resolved issues
		for id, prevInfo := range p.prevIssueIDs {
			if _, exists := currentIDs[id]; !exists {
				displayID := prevInfo.identifier
				if displayID == "" {
					displayID = id
				}
				sdk.Emit(sdk.Event{
					Type:     "issue.resolved",
					PluginID: pluginID,
					Message:  fmt.Sprintf("Issue %s completed or moved", displayID),
					Severity: sdk.SeverityInfo,
					Data:     map[string]string{"issueId": id, "kind": prevInfo.kind, "url": prevInfo.url},
				})
			}
		}

		// Detect new overdue issues
		if currentOverdue > p.prevOverdueCount {
			sdk.Emit(sdk.Event{
				Type:     "issue.overdue",
				PluginID: pluginID,
				Message:  fmt.Sprintf("%d overdue issue(s) need attention", currentOverdue),
				Severity: sdk.SeverityWarning,
				Data:     map[string]string{"count": strconv.Itoa(currentOverdue)},
			})
		}
	}

	p.prevIssueIDs = currentIDs
	p.prevOverdueCount = currentOverdue
	p.seeded = true
}

func (p *linearHandler) fetchData() (graphqlData, bool, error) {
	query, variables := p.buildQuery()
	if query == "" {
		// Every source is disabled; an empty selection set is invalid GraphQL.
		return graphqlData{}, false, nil
	}
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return graphqlData{}, false, err
	}
	sdk.Log("graphql query=%s", query)

	endpoint := apiURL
	if p.apiURL != "" {
		endpoint = p.apiURL
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return graphqlData{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", p.token)
	req.Header.Set("User-Agent", "smuler-linear-plugin/"+pluginVersion)

	resp, err := p.client.Do(req)
	if err != nil {
		return graphqlData{}, false, err
	}
	defer resp.Body.Close()

	p.lastHTTPStatus = resp.StatusCode
	p.lastRetryAfter = retryAfterSeconds(resp.Header)

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return graphqlData{}, false, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		p.lastError = "Linear API rate limited"
		return graphqlData{}, true, fmt.Errorf("rate limited")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		p.lastError = "Linear authentication failed"
		return graphqlData{}, false, fmt.Errorf("auth required")
	}

	// Linear reports rate limits and auth failures as GraphQL errors (often
	// with HTTP 400), identified by extensions.code.
	var parsed graphqlResponse
	parseErr := json.Unmarshal(data, &parsed)
	if len(parsed.Errors) > 0 {
		p.lastError = parsed.Errors[0].Message
		for _, gqlErr := range parsed.Errors {
			code := strings.ToUpper(gqlErr.Extensions.Code)
			msg := strings.ToLower(gqlErr.Message)
			if code == "RATELIMITED" || strings.Contains(msg, "rate limit") {
				p.lastError = "Linear API rate limited"
				return graphqlData{}, true, fmt.Errorf("rate limited")
			}
			if code == "AUTHENTICATION_ERROR" || code == "UNAUTHENTICATED" {
				p.lastError = "Linear authentication failed"
				p.lastHTTPStatus = http.StatusUnauthorized
				return graphqlData{}, false, fmt.Errorf("auth required")
			}
		}
		return graphqlData{}, false, fmt.Errorf("%s", parsed.Errors[0].Message)
	}
	if resp.StatusCode >= 400 {
		p.lastError = fmt.Sprintf("Linear API error (%d)", resp.StatusCode)
		return graphqlData{}, false, fmt.Errorf("linear api status %d", resp.StatusCode)
	}
	if parseErr != nil {
		return graphqlData{}, false, parseErr
	}
	return parsed.Data, false, nil
}

// retryAfterSeconds honours Retry-After, falling back to Linear's
// X-RateLimit-Requests-Reset header (UTC epoch milliseconds).
func retryAfterSeconds(h http.Header) int {
	if v := httphealth.ParseRetryAfter(h.Get("Retry-After")); v > 0 {
		return v
	}
	if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Requests-Reset"), 10, 64); err == nil && reset > 0 {
		if d := time.Until(time.UnixMilli(reset)); d > 0 {
			return int(d.Seconds()) + 1
		}
	}
	return 0
}

func (p *linearHandler) buildQuery() (string, map[string]interface{}) {
	sections := []string{}
	variables := map[string]interface{}{}

	if p.config.ShowAssigned {
		sections = append(sections, `viewer {
    assignedIssues(first: 25, filter: { state: { type: { nin: ["completed", "canceled"] } } }) {
      nodes { id identifier title url updatedAt dueDate priority state { name type } team { id name key } }
    }
  }`)
	}

	if p.config.ShowMentions {
		// issue only exists on IssueNotification, so it must be queried via an
		// inline fragment; mentions and read state are filtered client-side.
		sections = append(sections, `notifications(first: 25) {
    nodes { id type updatedAt readAt ... on IssueNotification { issue { id identifier title url updatedAt dueDate priority state { name type } team { id name key } } } }
  }`)
	}

	if p.config.ShowTriage && len(p.config.TeamIDs) > 0 {
		variables["teamIds"] = p.config.TeamIDs
		sections = append(sections, `triageIssues: issues(first: 10, filter: { assignee: { null: true }, team: { key: { in: $teamIds } }, state: { type: { nin: ["completed", "canceled"] } } }) {
    nodes { id identifier title url updatedAt dueDate priority state { name type } team { id name key } }
  }`)
	}

	if len(sections) == 0 {
		return "", nil
	}

	variableDecls := []string{}
	if _, ok := variables["teamIds"]; ok {
		variableDecls = append(variableDecls, "$teamIds: [String!]")
	}

	query := "query SmulerLinear"
	if len(variableDecls) > 0 {
		query += "(" + strings.Join(variableDecls, ", ") + ")"
	}
	query += " {\n  " + strings.Join(sections, "\n  ") + "\n}"

	return query, variables
}

func collectSignals(data graphqlData, cfg linearConfig) []signal {
	result := make([]signal, 0)
	now := time.Now()
	if cfg.ShowAssigned {
		for _, issue := range data.Viewer.Assigned.Nodes {
			severity, detail, alert := issueUrgency(issue, now)
			result = append(result, signal{kind: "assigned", issue: issue, updatedAt: parseTime(issue.UpdatedAt), severity: severity, detail: detail, alert: alert, uniqueID: issue.ID})
		}
	}
	if cfg.ShowMentions {
		for _, note := range data.Notifications.Nodes {
			if note.Issue == nil || note.ReadAt != nil || !strings.Contains(strings.ToLower(note.Type), "mention") {
				continue
			}
			result = append(result, signal{kind: "mention", issue: *note.Issue, updatedAt: parseTime(note.UpdatedAt), severity: "warning", detail: "Unread mention", alert: "You have unread Linear mentions", uniqueID: note.ID})
		}
	}
	if cfg.ShowTriage && len(cfg.TeamIDs) > 0 {
		for _, issue := range data.Triage.Nodes {
			severity := "info"
			detail := "Needs triage"
			alert := ""
			if isHighPriority(issue.Priority) {
				severity = "warning"
				detail = "High priority unassigned"
				alert = "High priority Linear issue needs triage"
			}
			result = append(result, signal{kind: "triage", issue: issue, updatedAt: parseTime(issue.UpdatedAt), severity: severity, detail: detail, alert: alert, uniqueID: issue.ID})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		li, lj := signalScore(result[i]), signalScore(result[j])
		if li != lj {
			return li > lj
		}
		return result[i].updatedAt.After(result[j].updatedAt)
	})
	return result
}

func signalScore(s signal) int {
	score := 0
	switch s.severity {
	case "critical":
		score += 100
	case "warning":
		score += 50
	}
	switch s.kind {
	case "assigned":
		score += 20
	case "mention":
		score += 15
	case "triage":
		score += 10
	}
	return score
}

func issueUrgency(issue issueNode, now time.Time) (string, string, string) {
	severity := "info"
	parts := make([]string, 0, 4)
	if issue.Team.Key != "" {
		parts = append(parts, issue.Team.Key)
	}
	if issue.State.Name != "" {
		parts = append(parts, issue.State.Name)
	}
	alert := ""
	if issue.DueDate != "" {
		if due, err := time.Parse("2006-01-02", issue.DueDate); err == nil {
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			dueDay := time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, now.Location())
			switch {
			case dueDay.Before(today):
				severity = "warning"
				parts = append(parts, "Overdue")
				alert = fmt.Sprintf("%s is overdue", issueTitle(issue))
			case dueDay.Equal(today):
				severity = "warning"
				parts = append(parts, "Due today")
				alert = fmt.Sprintf("%s is due today", issueTitle(issue))
			default:
				parts = append(parts, "Due "+due.Format("Jan 2"))
			}
		}
	}
	if isHighPriority(issue.Priority) {
		if severity == "info" {
			severity = "warning"
		}
		parts = append(parts, priorityLabel(issue.Priority))
	}
	return severity, strings.Join(parts, " · "), alert
}

// Linear priorities: 0 = none, 1 = urgent, 2 = high, 3 = medium, 4 = low.
func isHighPriority(priority int) bool {
	return priority == 1 || priority == 2
}

func priorityLabel(priority int) string {
	switch priority {
	case 1:
		return "Urgent"
	case 2:
		return "High priority"
	case 3:
		return "Medium priority"
	case 4:
		return "Low priority"
	default:
		return ""
	}
}

func toItem(s signal) sdk.Item {
	return sdk.Item{
		ID:        s.uniqueID,
		Title:     issueTitle(s.issue),
		Subtitle:  itemSubtitle(s),
		Detail:    s.detail,
		Severity:  s.severity,
		Timestamp: isoTimestamp(s.issue.UpdatedAt),
		DeepLink:  s.issue.URL,
		Actions:   []sdk.Action{{ID: "open", Label: "Open Issue"}},
	}
}

func itemSubtitle(s signal) string {
	parts := []string{}
	if s.issue.Team.Name != "" {
		parts = append(parts, s.issue.Team.Name)
	}
	switch s.kind {
	case "mention":
		parts = append(parts, "Mention")
	case "triage":
		parts = append(parts, "Triage")
	}
	if len(parts) == 0 {
		return s.issue.State.Name
	}
	return strings.Join(parts, " · ")
}

func issueTitle(issue issueNode) string {
	if issue.Identifier == "" {
		return issue.Title
	}
	return issue.Identifier + " " + issue.Title
}

// isoTimestamp normalises Linear's millisecond timestamps to RFC3339 without
// fractional seconds, which is what the host's ISO8601 parser accepts.
func isoTimestamp(raw string) string {
	t := parseTime(raw)
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func main() {
	sdk.Run(pluginID, pluginVersion, &linearHandler{
		client:       &http.Client{Timeout: 12 * time.Second},
		config:       linearConfig{ShowAssigned: true},
		prevIssueIDs: make(map[string]prevIssueInfo),
	})
}
