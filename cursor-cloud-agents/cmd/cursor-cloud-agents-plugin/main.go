package main

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/cursor-cloud-agents/internal/cursorapi"
	"github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "cursor-cloud-agents"
	pluginVersion = "0.1.0"
)

type handler struct {
	client *cursorapi.Client

	mu sync.Mutex

	apiKey          string
	refreshSeconds  int
	maxAgents       int
	includeArchived bool
	detailLevel     string

	prevRunStatus map[string]string
	userLabel     string
}

func main() {
	sdk.Run(pluginID, pluginVersion, &handler{
		client:        cursorapi.NewClient("", &http.Client{Timeout: 12 * time.Second}),
		prevRunStatus: make(map[string]string),
	})
}

func (h *handler) Initialize(params sdk.InitializeParams) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.apiKey = extractAPIKey(params)
	h.client.SetAPIKey(h.apiKey)

	h.refreshSeconds = parseInt(params.Config["refreshSeconds"], 60)
	h.maxAgents = parseInt(params.Config["maxAgents"], 20)
	h.includeArchived = parseBool(params.Config["includeArchived"], false)
	h.detailLevel = strings.TrimSpace(params.Config["detailLevel"])
	if h.detailLevel == "" {
		h.detailLevel = "full"
	}

	if h.apiKey == "" {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

func (h *handler) GetStatus() sdk.Snapshot {
	h.mu.Lock()
	apiKey := h.apiKey
	refreshSeconds := h.refreshSeconds
	maxAgents := h.maxAgents
	includeArchived := h.includeArchived
	detailLevel := h.detailLevel
	h.mu.Unlock()

	if apiKey == "" {
		return authRequiredSnapshot(refreshSeconds)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	me, err := h.client.Me(ctx)
	if err != nil {
		return errorSnapshot(err, refreshSeconds)
	}

	userLabel := formatUserLabel(me)
	h.mu.Lock()
	h.userLabel = userLabel
	h.mu.Unlock()

	agents, err := h.client.ListAllAgents(ctx, cursorapi.ListOptions{
		IncludeArchived: includeArchived,
	})
	if err != nil {
		return errorSnapshot(err, refreshSeconds)
	}

	sortAgents(agents)

	displayLimit := maxAgents
	if displayLimit <= 0 {
		displayLimit = len(agents)
	}
	if displayLimit > len(agents) {
		displayLimit = len(agents)
	}

	displayAgents := agents
	if displayLimit < len(agents) {
		displayAgents = agents[:displayLimit]
	}

	enriched := h.client.EnrichAgents(ctx, displayAgents, cursorapi.EnrichOptions{
		DetailLevel: detailLevel,
		MaxAgents:   displayLimit,
		Concurrency: 4,
	})

	h.emitRunTransitions(enriched)

	items := make([]sdk.Item, 0, len(enriched))
	for _, agent := range enriched {
		items = append(items, agentToItem(agent))
	}

	running, errors := countRunStates(agents, enriched, detailLevel)
	total := len(agents)

	summaryValue, summarySeverity := formatSummary(running, errors, total)
	alerts := buildAlerts(running, errors, total, userLabel)

	health := sdk.HealthOK
	if errors > 0 && running == 0 {
		health = sdk.HealthDegraded
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateReady,
		Summary: sdk.Summary{
			Title:    "Cursor Cloud Agents",
			Value:    summaryValue,
			Trend:    sdk.TrendSteady,
			Severity: summarySeverity,
			IconHint: "cloud",
		},
		Items:        items,
		Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: refreshSeconds,
		Health:       health,
	}
}

func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
	switch id {
	case "refresh":
		return true, ""
	case "open":
		url := params["url"]
		if url == "" {
			return false, "missing url"
		}
		if err := exec.Command("open", url).Run(); err != nil {
			return false, fmt.Sprintf("failed to open url: %v", err)
		}
		return true, ""
	default:
		if strings.HasPrefix(id, "open-pr:") {
			prURL := strings.TrimPrefix(id, "open-pr:")
			if prURL == "" {
				return false, "missing pr url"
			}
			if err := exec.Command("open", prURL).Run(); err != nil {
				return false, fmt.Sprintf("failed to open pr: %v", err)
			}
			return true, ""
		}
		return false, "unknown action: " + id
	}
}

func (h *handler) Shutdown() {}

func (h *handler) emitRunTransitions(agents []cursorapi.EnrichedAgent) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, agent := range agents {
		runStatus := runStatusFor(agent)
		if runStatus == "" {
			continue
		}

		prev, seen := h.prevRunStatus[agent.List.ID]
		if seen && prev == runStatus {
			continue
		}
		h.prevRunStatus[agent.List.ID] = runStatus

		if !seen {
			continue
		}

		name := agent.List.Name
		if name == "" {
			name = agent.List.ID
		}

		switch {
		case isActiveRunStatus(runStatus) && !isActiveRunStatus(prev):
			sdk.Emit(sdk.Event{
				Type:      "agent.started",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s is now running", name),
				Severity:  sdk.SeverityInfo,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Data: map[string]string{
					"agentId":   agent.List.ID,
					"runStatus": runStatus,
					"url":       agent.List.URL,
				},
			})
		case runStatus == "FINISHED" && prev != "FINISHED":
			sdk.Emit(sdk.Event{
				Type:      "agent.run_finished",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s finished", name),
				Severity:  sdk.SeverityInfo,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Data: map[string]string{
					"agentId":   agent.List.ID,
					"runStatus": runStatus,
					"url":       agent.List.URL,
				},
			})
		case isErrorRunStatus(runStatus) && !isErrorRunStatus(prev):
			sdk.Emit(sdk.Event{
				Type:      "agent.run_error",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s failed (%s)", name, runStatus),
				Severity:  sdk.SeverityCritical,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Data: map[string]string{
					"agentId":   agent.List.ID,
					"runStatus": runStatus,
					"url":       agent.List.URL,
				},
			})
		}
	}
}

func extractAPIKey(params sdk.InitializeParams) string {
	if params.Auth != nil {
		if key := strings.TrimSpace(params.Auth.AccountID); key != "" {
			return key
		}
	}
	for _, pa := range params.ProviderAuths {
		if pa.Kind == "api_key" && pa.APIKey != "" {
			return strings.TrimSpace(pa.APIKey)
		}
	}
	return ""
}

func parseInt(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func parseBool(raw string, fallback bool) bool {
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func authRequiredSnapshot(refreshAfter int) sdk.Snapshot {
	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateDegraded,
		Summary: sdk.Summary{
			Title:    "Cursor Cloud Agents",
			Value:    "Auth required",
			Trend:    sdk.TrendSteady,
			Severity: sdk.SeverityInfo,
			IconHint: "cloud",
		},
		Items:   []sdk.Item{},
		Actions: []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts: []sdk.Alert{{
			ID:       "cursor-cloud-agents-auth",
			Severity: sdk.SeverityInfo,
			Message:  "Add a Cursor API key in Settings → Credentials (generate at cursor.com/dashboard → API Keys)",
		}},
		RefreshAfter: refreshAfter,
		Health:       sdk.HealthAuthReq,
	}
}

func errorSnapshot(err error, refreshAfter int) sdk.Snapshot {
	health := sdk.HealthDegraded
	refresh := refreshAfter
	if apiErr, ok := err.(*cursorapi.APIError); ok {
		if apiErr.IsAuth() {
			return authRequiredSnapshot(refreshAfter)
		}
		if apiErr.IsRateLimited() {
			health = sdk.HealthRateLimited
			if refresh > 120 {
				refresh = 120
			}
		}
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateDegraded,
		Summary: sdk.Summary{
			Title:    "Cursor Cloud Agents",
			Value:    "Unavailable",
			Trend:    sdk.TrendSteady,
			Severity: sdk.SeverityInfo,
			IconHint: "cloud",
		},
		Items:   []sdk.Item{},
		Actions: []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts: []sdk.Alert{{
			ID:       "cursor-cloud-agents-error",
			Severity: sdk.SeverityInfo,
			Message:  fmt.Sprintf("Cursor API error: %v", err),
		}},
		RefreshAfter: refresh,
		Health:       health,
	}
}

func formatUserLabel(me *cursorapi.MeResponse) string {
	if me == nil {
		return ""
	}
	name := strings.TrimSpace(strings.TrimSpace(me.UserFirstName + " " + me.UserLastName))
	if name != "" && me.UserEmail != "" {
		return name + " · " + me.UserEmail
	}
	if me.UserEmail != "" {
		return me.UserEmail
	}
	if me.APIKeyName != "" {
		return me.APIKeyName
	}
	return ""
}

func sortAgents(agents []cursorapi.AgentListItem) {
	sort.SliceStable(agents, func(i, j int) bool {
		ri := runRank(agents[i])
		rj := runRank(agents[j])
		if ri != rj {
			return ri < rj
		}
		ti := cursorapi.ParseTime(agents[i].UpdatedAt)
		tj := cursorapi.ParseTime(agents[j].UpdatedAt)
		return ti.After(tj)
	})
}

func runRank(agent cursorapi.AgentListItem) int {
	switch strings.ToUpper(agent.Status) {
	case "ACTIVE":
		return 0
	default:
		return 1
	}
}

func runStatusFor(agent cursorapi.EnrichedAgent) string {
	if agent.Run != nil && agent.Run.Status != "" {
		return strings.ToUpper(agent.Run.Status)
	}
	return strings.ToUpper(agent.List.Status)
}

func isActiveRunStatus(status string) bool {
	switch strings.ToUpper(status) {
	case "RUNNING", "CREATING":
		return true
	default:
		return false
	}
}

func isErrorRunStatus(status string) bool {
	switch strings.ToUpper(status) {
	case "ERROR", "CANCELLED", "EXPIRED":
		return true
	default:
		return false
	}
}

func countRunStates(all []cursorapi.AgentListItem, enriched []cursorapi.EnrichedAgent, detailLevel string) (running, errors int) {
	if detailLevel == "full" && len(enriched) > 0 {
		enrichedByID := make(map[string]cursorapi.EnrichedAgent, len(enriched))
		for _, a := range enriched {
			enrichedByID[a.List.ID] = a
		}
		for _, agent := range all {
			if e, ok := enrichedByID[agent.ID]; ok {
				status := runStatusFor(e)
				if isActiveRunStatus(status) {
					running++
				}
				if isErrorRunStatus(status) {
					errors++
				}
				continue
			}
			if strings.EqualFold(agent.Status, "ACTIVE") {
				running++
			}
		}
		return running, errors
	}

	for _, agent := range all {
		if strings.EqualFold(agent.Status, "ACTIVE") {
			running++
		}
	}
	return running, errors
}

func formatSummary(running, errors, total int) (value, severity string) {
	if total == 0 {
		return "No agents", sdk.SeverityInfo
	}
	if running > 0 {
		severity = sdk.SeverityWarning
		value = fmt.Sprintf("%d running · %d total", running, total)
		return value, severity
	}
	if errors > 0 {
		return fmt.Sprintf("%d errors · %d total", errors, total), sdk.SeverityWarning
	}
	return fmt.Sprintf("%d total", total), sdk.SeverityInfo
}

func buildAlerts(running, errors, total int, userLabel string) []sdk.Alert {
	alerts := make([]sdk.Alert, 0, 3)
	if userLabel != "" {
		alerts = append(alerts, sdk.Alert{
			ID:       "cursor-cloud-agents-user",
			Severity: sdk.SeverityInfo,
			Message:  userLabel,
		})
	}
	if running > 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "cursor-cloud-agents-running",
			Severity: sdk.SeverityWarning,
			Message:  fmt.Sprintf("%d cloud agent run(s) in progress", running),
		})
	}
	if errors > 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "cursor-cloud-agents-errors",
			Severity: sdk.SeverityCritical,
			Message:  fmt.Sprintf("%d cloud agent run(s) in error state", errors),
		})
	}
	if total == 0 && len(alerts) == 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "cursor-cloud-agents-empty",
			Severity: sdk.SeverityInfo,
			Message:  "No cloud agents found for this API key",
		})
	}
	return alerts
}

func agentToItem(agent cursorapi.EnrichedAgent) sdk.Item {
	list := agent.List
	runStatus := runStatusFor(agent)
	subtitle := list.Status
	if runStatus != "" && !strings.EqualFold(runStatus, list.Status) {
		subtitle = list.Status + " · run " + runStatus
	}

	detail := buildDetail(agent)
	severity := itemSeverity(runStatus, list.Status)
	actions := []sdk.Action{{ID: "open", Label: "Open Agent"}}
	if prURL := firstPRURL(agent); prURL != "" {
		actions = append(actions, sdk.Action{ID: "open-pr:" + prURL, Label: "Open PR"})
	}

	title := list.Name
	if title == "" {
		title = list.ID
	}

	return sdk.Item{
		ID:        list.ID,
		Title:     title,
		Subtitle:  subtitle,
		Detail:    detail,
		Severity:  severity,
		Timestamp: list.UpdatedAt,
		DeepLink:  list.URL,
		Actions:   actions,
		Metadata: map[string]string{
			"agentStatus": list.Status,
			"runStatus":   runStatus,
		},
	}
}

func itemSeverity(runStatus, agentStatus string) string {
	if isErrorRunStatus(runStatus) {
		return sdk.SeverityCritical
	}
	if isActiveRunStatus(runStatus) || strings.EqualFold(agentStatus, "ACTIVE") {
		return sdk.SeverityWarning
	}
	return sdk.SeverityInfo
}

func buildDetail(agent cursorapi.EnrichedAgent) string {
	parts := make([]string, 0, 6)

	if agent.Detail != nil && len(agent.Detail.Repos) > 0 {
		repo := agent.Detail.Repos[0]
		repoLine := repo.URL
		if repo.StartingRef != "" {
			repoLine += " @ " + repo.StartingRef
		}
		parts = append(parts, repoLine)
	} else if agent.List.Env.Type != "" {
		parts = append(parts, "env: "+agent.List.Env.Type)
	}

	if agent.Run != nil && len(agent.Run.Git.Branches) > 0 {
		branch := agent.Run.Git.Branches[0]
		if branch.Branch != "" {
			parts = append(parts, "branch: "+branch.Branch)
		}
		if branch.PRURL != "" {
			parts = append(parts, "pr: "+branch.PRURL)
		}
	}

	if agent.Run != nil && agent.Run.Result != "" {
		parts = append(parts, truncate(agent.Run.Result, 160))
	}

	if agent.Usage != nil && agent.Usage.TotalUsage.TotalTokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens: %d", agent.Usage.TotalUsage.TotalTokens))
	}

	if len(parts) == 0 {
		if agent.List.LatestRunID != "" {
			return "latest run: " + agent.List.LatestRunID
		}
		return agent.List.URL
	}
	return strings.Join(parts, " · ")
}

func firstPRURL(agent cursorapi.EnrichedAgent) string {
	if agent.Run == nil {
		return ""
	}
	for _, b := range agent.Run.Git.Branches {
		if b.PRURL != "" {
			return b.PRURL
		}
	}
	return ""
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
