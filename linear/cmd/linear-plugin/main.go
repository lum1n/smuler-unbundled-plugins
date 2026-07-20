package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	"github.com/lum1n/smuler/plugins/plugindebug"
)

func logDebug(format string, args ...interface{}) {
	plugindebug.Log("[linear-plugin]", format, args...)
}

const (
	apiURL          = "https://api.linear.app/graphql"
	protocolVersion = "0.1.0"
	pluginVersion   = "0.1.0"
)

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
	ProtocolVersion string            `json:"protocolVersion"`
	PluginID        string            `json:"pluginId"`
	Config          map[string]string `json:"config"`
	Auth            *authContext      `json:"auth"`
}

type authContext struct {
	AccountID string `json:"accountId"`
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

type linearConfig struct {
	ShowAssigned bool
	ShowMentions bool
	ShowTriage   bool
	TeamIDs      []string
}

type linearPlugin struct {
	token          string
	config         linearConfig
	client         *http.Client
	lastError      string
	lastRetryAfter int
	lastHTTPStatus int

	// State tracking for delta detection
	prevIssueIDs     map[string]prevIssueInfo // id -> previous state
	prevOverdueCount int
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
	Message string `json:"message"`
	Type    string `json:"type"`
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

func main() {
	defer func() {
		if r := recover(); r != nil {
			logDebug("panic: %v", r)
		}
	}()

	pl := &linearPlugin{
		client:        &http.Client{Timeout: 12 * time.Second},
		config:        linearConfig{ShowAssigned: true},
		prevIssueIDs:  make(map[string]prevIssueInfo),
	}
	logDebug("started pwd=%s", mustGetwd())

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
		logDebug("parsed method=%s id=%d", req.Method, req.ID)

		switch req.Method {
		case "initialize":
			if err := pl.handleInitialize(req); err != nil {
				logDebug("initialize failed: %v", err)
				sendError(req.ID, -32000, err.Error(), false, "Check Linear token and settings")
			}
		case "getStatus", "refresh":
			logDebug("building snapshot")
			sendResult(req.ID, pl.buildSnapshot())
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

func (p *linearPlugin) handleInitialize(req rpcRequest) error {
	var params initializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fmt.Errorf("decode initialize: %w", err)
	}
	plugindebug.ConfigureFromInitializeConfig(params.Config)
	if params.Auth != nil {
		p.token = strings.TrimSpace(params.Auth.AccountID)
	}
	p.config = parseConfig(params.Config)
	logDebug("initialize token=%t assigned=%t mentions=%t triage=%t teamIds=%q", p.token != "", p.config.ShowAssigned, p.config.ShowMentions, p.config.ShowTriage, strings.Join(p.config.TeamIDs, ","))
	sendResult(req.ID, initializedPayload{
		Type:            "initialized",
		ProtocolVersion: protocolVersion,
		PluginVersion:   pluginVersion,
		Health:          "ok",
	})
	return nil
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

func (p *linearPlugin) buildSnapshot() pluginSnapshot {
	if strings.TrimSpace(p.token) == "" {
		return emptySnapshot("No token configured", "auth_required", 0)
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
			return emptySnapshot(message, "rate_limited", p.lastRetryAfter)
		}
		if p.lastHTTPStatus == http.StatusUnauthorized || p.lastHTTPStatus == http.StatusForbidden {
			return emptySnapshot("Linear authentication failed — reconnect in Settings", "auth_required", p.lastRetryAfter)
		}
		return emptySnapshot(message, "degraded", p.lastRetryAfter)
	}

	signals := collectSignals(data, p.config)

	// Emit events for state changes
	p.emitDeltaEvents(signals)

	if len(signals) == 0 {
		return pluginSnapshot{
			PluginID: "linear",
			State:    "ready",
			Summary: pluginSummary{Title: "Linear", Value: "No active issues", Trend: "steady", Severity: "info", IconHint: "circle.grid.2x2"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{{ID: "refresh", Label: "Refresh"}},
			Alerts:       []pluginAlert{},
			RefreshAfter: 300,
			Health:       "ok",
		}
	}

	items := make([]pluginItem, 0, len(signals))
	alerts := make([]pluginAlert, 0)
	alertIDs := map[string]struct{}{}
	overdueCount := 0
	mentionCount := 0
	triageCount := 0

	for _, signal := range signals {
		items = append(items, toItem(signal))
		if signal.alert != "" {
			if _, ok := alertIDs[signal.alert]; !ok {
				alertIDs[signal.alert] = struct{}{}
				alerts = append(alerts, pluginAlert{ID: signal.uniqueID + "-alert", Severity: signal.severity, Message: signal.alert})
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

	severity := "info"
	value := fmt.Sprintf("%d assigned", countKind(signals, "assigned"))
	trend := "steady"
	if overdueCount > 0 {
		severity = "warning"
		value = fmt.Sprintf("%d overdue", overdueCount)
	}
	if overdueCount == 0 && mentionCount > 0 {
		value = fmt.Sprintf("%d mentions", mentionCount)
		trend = "up"
	}
	if overdueCount == 0 && mentionCount == 0 && triageCount > 0 {
		value = fmt.Sprintf("%d triage", triageCount)
	}

	return pluginSnapshot{
		PluginID: "linear",
		State:    "ready",
		Summary:  pluginSummary{Title: "Linear", Value: value, Trend: trend, Severity: severity, IconHint: "circle.grid.2x2"},
		Items:    items,
		Actions:  []pluginAction{{ID: "refresh", Label: "Refresh"}},
		Alerts:   alerts,
		RefreshAfter: 300,
		Health:       "ok",
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

func emptySnapshot(message, health string, retryAfter int) pluginSnapshot {
	return pluginSnapshot{
		PluginID:     "linear",
		State:        "degraded",
		Summary:      pluginSummary{Title: "Linear", Value: message, Trend: "steady", Severity: "info", IconHint: "circle.grid.2x2"},
		Items:        []pluginItem{},
		Actions:      []pluginAction{},
		Alerts:       []pluginAlert{{ID: "linear-config", Severity: "info", Message: message}},
		RefreshAfter: httphealth.DefaultRefreshAfter(health, retryAfter),
		Health:       health,
	}
}

func (p *linearPlugin) emitDeltaEvents(signals []signal) {
	now := time.Now().UTC().Format(time.RFC3339)
	currentIDs := make(map[string]prevIssueInfo) // id -> info
	currentOverdue := 0

	for _, s := range signals {
		currentIDs[s.uniqueID] = prevIssueInfo{kind: s.kind, url: s.issue.URL, identifier: s.issue.Identifier}
		if strings.Contains(strings.ToLower(s.detail), "overdue") {
			currentOverdue++
		}
	}

	if len(p.prevIssueIDs) > 0 {
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
				sendNotification("event", eventParams{
					Event: pluginEvent{
						Type:      "issue.created",
						PluginID:  "linear",
						Message:   label,
						Severity:  "info",
						Data:      map[string]string{"issueId": id, "kind": info.kind, "url": info.url},
						Timestamp: now,
					},
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
				sendNotification("event", eventParams{
					Event: pluginEvent{
						Type:      "issue.resolved",
						PluginID:  "linear",
						Message:   fmt.Sprintf("Issue %s completed or moved", displayID),
						Severity:  "info",
						Data:      map[string]string{"issueId": id, "kind": prevInfo.kind, "url": prevInfo.url},
						Timestamp: now,
					},
				})
			}
		}

		// Detect new overdue issues
		if currentOverdue > p.prevOverdueCount {
			sendNotification("event", eventParams{
				Event: pluginEvent{
					Type:      "issue.overdue",
					PluginID:  "linear",
					Message:   fmt.Sprintf("%d overdue issue(s) need attention", currentOverdue),
					Severity:  "warning",
					Data:      map[string]string{"count": strconv.Itoa(currentOverdue)},
					Timestamp: now,
				},
			})
		}
	}

	p.prevIssueIDs = currentIDs
	p.prevOverdueCount = currentOverdue
}

func (p *linearPlugin) fetchData() (graphqlData, bool, error) {
	query, variables := p.buildQuery()
	body, err := json.Marshal(graphqlRequest{Query: query, Variables: variables})
	if err != nil {
		return graphqlData{}, false, err
	}
 	logDebug("graphql query=%s", query)

	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return graphqlData{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", p.token)
	req.Header.Set("User-Agent", "smuler-linear-plugin/0.1.0")

	resp, err := p.client.Do(req)
	if err != nil {
		return graphqlData{}, false, err
	}
	defer resp.Body.Close()

	p.lastHTTPStatus = resp.StatusCode
	p.lastRetryAfter = httphealth.ParseRetryAfter(resp.Header.Get("Retry-After"))

	data, err := io.ReadAll(resp.Body)
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
	if resp.StatusCode >= 400 {
		p.lastError = extractGraphQLError(data)
		if p.lastError == "" {
			p.lastError = fmt.Sprintf("Linear API error (%d)", resp.StatusCode)
		}
		return graphqlData{}, false, fmt.Errorf("linear api status %d", resp.StatusCode)
	}

	var parsed graphqlResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return graphqlData{}, false, err
	}
	if len(parsed.Errors) > 0 {
		rateLimited := false
		p.lastError = parsed.Errors[0].Message
		for _, gqlErr := range parsed.Errors {
			if strings.Contains(strings.ToLower(gqlErr.Message), "rate") {
				rateLimited = true
			}
		}
		return graphqlData{}, rateLimited, fmt.Errorf(parsed.Errors[0].Message)
	}
	return parsed.Data, false, nil
}

func (p *linearPlugin) buildQuery() (string, map[string]interface{}) {
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
		sections = append(sections, `notifications(first: 10) {
    nodes { id type updatedAt issue { id identifier title url updatedAt dueDate priority state { name type } team { id name key } } }
  }`)
	}

	if p.config.ShowTriage && len(p.config.TeamIDs) > 0 {
		variables["teamIds"] = p.config.TeamIDs
		sections = append(sections, `triageIssues: issues(first: 10, filter: { assignee: { null: true }, team: { key: { in: $teamIds } } }) {
    nodes { id identifier title url updatedAt dueDate priority state { name type } team { id name key } }
  }`)
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

func extractGraphQLError(data []byte) string {
	var parsed struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	if len(parsed.Errors) == 0 {
		return ""
	}
	return parsed.Errors[0].Message
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
			if note.Issue == nil {
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
			if issue.Priority >= 3 {
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
	if issue.Priority >= 3 {
		if severity == "info" {
			severity = "warning"
		}
		parts = append(parts, priorityLabel(issue.Priority))
	}
	return severity, strings.Join(parts, " · "), alert
}

func priorityLabel(priority int) string {
	if priority <= 0 {
		return ""
	}
	return fmt.Sprintf("Priority %d", priority)
}

func toItem(s signal) pluginItem {
	return pluginItem{
		ID:        s.uniqueID,
		Title:     issueTitle(s.issue),
		Subtitle:  itemSubtitle(s),
		Detail:    s.detail,
		Severity:  s.severity,
		Timestamp: s.issue.UpdatedAt,
		DeepLink:  s.issue.URL,
		Actions:   []pluginAction{{ID: "open", Label: "Open Issue"}},
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

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func sendResult(id int, result interface{}) {
	logDebug("sending result id=%d", id)
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		logDebug("json encode error: %v", err)
	}
}

func sendError(id int, code int, message string, retryable bool, suggestedAction string) {
	logDebug("sending error id=%d code=%d message=%s", id, code, message)
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    &pluginErrorData{Retryable: retryable, SuggestedAction: suggestedAction},
		},
	}
	if err := json.NewEncoder(os.Stdout).Encode(resp); err != nil {
		logDebug("json encode error: %v", err)
	}
}

func sendNotification(method string, params interface{}) {
	notif := struct {
		JSONRPC string      `json:"jsonrpc"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	if err := json.NewEncoder(os.Stdout).Encode(notif); err != nil {
		logDebug("json encode error: %v", err)
	}
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "unknown"
	}
	return wd
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
