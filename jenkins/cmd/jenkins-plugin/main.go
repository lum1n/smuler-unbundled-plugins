package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "jenkins"
	pluginVersion = "0.1.0"
	maxJobs       = 30
	maxDisplay    = 10
)

type jenkinsHandler struct {
	client *http.Client

	mu        sync.Mutex
	prevFails map[string]prevFailInfo
	prevCount int

	url    string
	user   string
	token  string
	filter *regexp.Regexp
}

type prevFailInfo struct {
	name string
	url  string
}

func (h *jenkinsHandler) Initialize(params sdk.InitializeParams) string {
	h.url = strings.TrimRight(params.Config["url"], "/")
	h.user = params.Config["user"]
	h.token = params.Config["token"]
	if f := params.Config["jobFilter"]; f != "" {
		r, err := regexp.Compile(f)
		if err != nil {
			sdk.Log("invalid jenkins.jobFilter regex: %v", err)
		} else {
			h.filter = r
		}
	}
	if h.url == "" {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

func (h *jenkinsHandler) GetStatus() sdk.Snapshot {
	if h.url == "" {
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title:    "Jenkins",
				Value:    "Not configured",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityInfo,
				IconHint: "workflow",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "jenkins-config", Severity: sdk.SeverityInfo, Message: "Set Jenkins URL and credentials in Settings"}},
			RefreshAfter: 300,
			Health:       sdk.HealthAuthReq,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jobs, err := h.listJobs(ctx)
	if err != nil {
		sdk.Log("listJobs error: %v", err)
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title:    "Jenkins",
				Value:    "Unreachable",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityInfo,
				IconHint: "workflow",
			},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "jenkins-error", Severity: sdk.SeverityInfo, Message: fmt.Sprintf("Cannot reach %s: %v", h.url, err)}},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	failing := make([]jenkinsJob, 0)
	for _, j := range jobs {
		if isFailing(j.Color) {
			failing = append(failing, j)
		}
	}

	h.emitDeltas(failing)

	sort.Slice(failing, func(i, j int) bool {
		return failing[i].LastBuild.Timestamp > failing[j].LastBuild.Timestamp
	})

	items := make([]sdk.Item, 0, maxDisplay)
	for _, j := range failing {
		if len(items) >= maxDisplay {
			break
		}
		severity := sdk.SeverityCritical
		if isUnstable(j.Color) {
			severity = sdk.SeverityWarning
		}
		items = append(items, sdk.Item{
			ID:        j.Name,
			Title:     j.Name,
			Subtitle:  colorLabel(j.Color),
			Detail:    buildDetail(j),
			Severity:  severity,
			Timestamp: millisToRFC3339(j.LastBuild.Timestamp),
			DeepLink:  j.URL,
			Actions:   []sdk.Action{{ID: "open", Label: "Open Job"}},
		})
	}

	alerts := []sdk.Alert{}
	if len(failing) > 0 {
		s := "s"
		if len(failing) == 1 {
			s = ""
		}
		alerts = append(alerts, sdk.Alert{
			ID:       "jenkins-failures",
			Severity: sdk.SeverityCritical,
			Message:  fmt.Sprintf("%d job%s failing", len(failing), s),
		})
	}

	var severity, value string
	if len(failing) > 0 {
		severity = sdk.SeverityCritical
		value = fmt.Sprintf("%d failing", len(failing))
	} else {
		severity = sdk.SeverityInfo
		value = "All passing"
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateReady,
		Summary: sdk.Summary{
			Title:    "Jenkins",
			Value:    value,
			Trend:    sdk.TrendSteady,
			Severity: severity,
			IconHint: "workflow",
		},
		Items:        items,
		Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: 120,
		Health:       sdk.HealthOK,
	}
}

func (h *jenkinsHandler) PerformAction(id string, params map[string]string) (bool, string) {
	return false, "unknown action: " + id
}

func (h *jenkinsHandler) Shutdown() {}

// --- Jenkins API ---

type jenkinsJob struct {
	Name      string       `json:"name"`
	URL       string       `json:"url"`
	Color     string       `json:"color"`
	LastBuild jenkinsBuild `json:"lastBuild"`
}

type jenkinsBuild struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Timestamp int64  `json:"timestamp"`
	Result    string `json:"result"`
	Building  bool   `json:"building"`
}

type jenkinsJobsResponse struct {
	Jobs []jenkinsJob `json:"jobs"`
}

func (h *jenkinsHandler) doRequest(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", h.url+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "smuler-jenkins-plugin/"+pluginVersion)
	if h.user != "" && h.token != "" {
		req.SetBasicAuth(h.user, h.token)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return io.ReadAll(resp.Body)
}

func (h *jenkinsHandler) listJobs(ctx context.Context) ([]jenkinsJob, error) {
	body, err := h.doRequest(ctx, "/api/json?tree=jobs[name,url,color,lastBuild[number,url,timestamp,result,building]]")
	if err != nil {
		return nil, err
	}
	var resp jenkinsJobsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse jobs response: %w", err)
	}

	if h.filter == nil {
		return resp.Jobs, nil
	}

	filtered := make([]jenkinsJob, 0, len(resp.Jobs))
	for _, j := range resp.Jobs {
		if h.filter.MatchString(j.Name) {
			filtered = append(filtered, j)
		}
	}
	return filtered, nil
}

// --- helpers ---

func isFailing(color string) bool {
	return strings.HasPrefix(color, "red") || strings.HasPrefix(color, "yellow")
}

func isUnstable(color string) bool {
	return strings.HasPrefix(color, "yellow")
}

func colorLabel(color string) string {
	switch {
	case strings.HasPrefix(color, "red_anime"):
		return "Failing (in progress)"
	case color == "red":
		return "Failed"
	case strings.HasPrefix(color, "yellow_anime"):
		return "Unstable (in progress)"
	case color == "yellow":
		return "Unstable"
	case strings.HasPrefix(color, "blue_anime"):
		return "Building"
	case color == "blue":
		return "Passing"
	case color == "aborted":
		return "Aborted"
	case color == "notbuilt":
		return "Not built"
	case color == "disabled":
		return "Disabled"
	default:
		return color
	}
}

func buildDetail(j jenkinsJob) string {
	parts := []string{}
	if j.LastBuild.Number > 0 {
		parts = append(parts, fmt.Sprintf("#%d", j.LastBuild.Number))
	}
	if j.LastBuild.Result != "" {
		parts = append(parts, j.LastBuild.Result)
	}
	if j.LastBuild.Building {
		parts = append(parts, "building...")
	}
	return strings.Join(parts, " ")
}

func millisToRFC3339(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.Unix(ms/1000, (ms%1000)*1e6).UTC().Format(time.RFC3339)
}

// --- delta events ---

func (h *jenkinsHandler) emitDeltas(jobs []jenkinsJob) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current := make(map[string]prevFailInfo)
	for _, j := range jobs {
		current[j.Name] = prevFailInfo{name: j.Name, url: j.URL}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for name, info := range h.prevFails {
		if _, ok := current[name]; !ok && len(h.prevFails) > 0 {
			sdk.Emit(sdk.Event{
				Type:      "build.fixed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s is passing again", info.name),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"jobName": info.name, "jobUrl": info.url},
				Timestamp: now,
			})
		}
	}

	for name, info := range current {
		if _, ok := h.prevFails[name]; !ok && len(h.prevFails) > 0 {
			sdk.Emit(sdk.Event{
				Type:      "build.failed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("%s build failed", info.name),
				Severity:  sdk.SeverityCritical,
				Data:      map[string]string{"jobName": info.name, "jobUrl": info.url},
				Timestamp: now,
			})
		}
	}

	newCount := len(current)
	if newCount != h.prevCount && len(h.prevFails) > 0 {
		if newCount > h.prevCount {
			sdk.Emit(sdk.Event{
				Type:      "failure.count_increased",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Failing jobs increased from %d to %d", h.prevCount, newCount),
				Severity:  sdk.SeverityCritical,
				Data:      map[string]string{"previous": strconv.Itoa(h.prevCount), "current": strconv.Itoa(newCount)},
				Timestamp: now,
			})
		} else if newCount < h.prevCount {
			sdk.Emit(sdk.Event{
				Type:      "failure.count_decreased",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Failing jobs decreased from %d to %d", h.prevCount, newCount),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"previous": strconv.Itoa(h.prevCount), "current": strconv.Itoa(newCount)},
				Timestamp: now,
			})
		}
	}

	h.prevFails = current
	h.prevCount = newCount
}

func main() {
	sdk.Run(pluginID, pluginVersion, &jenkinsHandler{
		client:    &http.Client{Timeout: 30 * time.Second},
		prevFails: make(map[string]prevFailInfo),
	})
}
