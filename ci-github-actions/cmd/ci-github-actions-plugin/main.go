package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID       = "ci-github-actions"
	pluginVersion   = "0.1.1"
	apiBase        = "https://api.github.com"
	maxConcurrency = 5
	maxRepos       = 30
	maxRunsPerRepo = 3
	maxDisplayRuns = 10
)

// --- GitHub API types ---

type ghRepo struct {
	ID       int    `json:"id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	PushedAt string `json:"pushed_at"`
}

type ghWorkflowRun struct {
	ID         json.Number `json:"id"`
	Name       string      `json:"name"`
	HTMLURL    string      `json:"html_url"`
	Status     string      `json:"status"`
	Conclusion string      `json:"conclusion"`
	CreatedAt  string      `json:"created_at"`
	UpdatedAt  string      `json:"updated_at"`
	HeadBranch string      `json:"head_branch"`
	HeadSHA    string      `json:"head_sha"`
	Event      string      `json:"event"`
	RunNumber  int         `json:"run_number"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type ghRunsResponse struct {
	TotalCount   int             `json:"total_count"`
	WorkflowRuns []ghWorkflowRun `json:"workflow_runs"`
}

// --- Plugin state ---

type prevFailInfo struct {
	title string
	url   string
}

type ciPlugin struct {
	token  string
	client *http.Client

	mu               sync.Mutex
	prevFails        map[string]prevFailInfo // run-N -> info
	prevFailureCount int
}

func (p *ciPlugin) Initialize(params sdk.InitializeParams) string {
	for _, auth := range params.ProviderAuths {
		if auth.AccountID != "" {
			p.token = auth.AccountID
			break
		}
	}
	if p.token == "" {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

func (p *ciPlugin) PerformAction(id string, params map[string]string) (bool, string) {
	return false, "unknown action: " + id
}

func (p *ciPlugin) Shutdown() {}

func (p *ciPlugin) apiRequest(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "smuler-ci-github-actions-plugin/"+pluginVersion)
	if p.token != "" {
		req.Header.Set("Authorization", "token "+p.token)
	}

	resp, err := p.client.Do(req)
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

func (p *ciPlugin) fetchRepos(ctx context.Context) ([]ghRepo, error) {
	body, err := p.apiRequest(ctx, "/user/repos?sort=pushed&per_page="+strconv.Itoa(maxRepos)+"&type=owner")
	if err != nil {
		return nil, err
	}

	var repos []ghRepo
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, fmt.Errorf("parse repos response: %w", err)
	}
	return repos, nil
}

func (p *ciPlugin) fetchRepoRuns(ctx context.Context, fullName string) ([]ghWorkflowRun, error) {
	path := fmt.Sprintf("/repos/%s/actions/runs?status=failure&per_page=%d", fullName, maxRunsPerRepo)
	body, err := p.apiRequest(ctx, path)
	if err != nil {
		return nil, err
	}

	var resp ghRunsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse runs response: %w", err)
	}
	return resp.WorkflowRuns, nil
}

func (p *ciPlugin) GetStatus() sdk.Snapshot {
	if p.token == "" {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateDegraded,
			Summary:      sdk.Summary{Title: "GitHub Actions", Value: "No token configured", Trend: sdk.TrendSteady, Severity: sdk.SeverityInfo, IconHint: "workflow"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "gha-config", Severity: sdk.SeverityInfo, Message: "No token configured"}},
			RefreshAfter: 300,
			Health:       sdk.HealthAuthReq,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repos, err := p.fetchRepos(ctx)
	if err != nil {
		msg := fmt.Sprintf("API unreachable: %v", err)
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateDegraded,
			Summary:      sdk.Summary{Title: "GitHub Actions", Value: msg, Trend: sdk.TrendSteady, Severity: sdk.SeverityInfo, IconHint: "workflow"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "gha-error", Severity: sdk.SeverityInfo, Message: msg}},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	allRuns := p.fetchAllRuns(ctx, repos)
	p.emitDeltaEvents(allRuns)

	sort.Slice(allRuns, func(i, j int) bool {
		return allRuns[i].CreatedAt > allRuns[j].CreatedAt
	})

	items := make([]sdk.Item, 0, maxDisplayRuns)
	for _, run := range allRuns {
		if len(items) >= maxDisplayRuns {
			break
		}
		runID := run.ID.String()
		branch := run.HeadBranch
		shortSHA := run.HeadSHA
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		subtitle := run.Repository.FullName
		detail := fmt.Sprintf("#%d %s @ %s", run.RunNumber, run.Event, branch)
		if shortSHA != "" {
			detail += " (" + shortSHA + ")"
		}

		items = append(items, sdk.Item{
			ID:        runID,
			Title:     run.Name,
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  sdk.SeverityCritical,
			Timestamp: run.CreatedAt,
			DeepLink:  run.HTMLURL,
			Actions:   []sdk.Action{{ID: "open", Label: "Open Run"}},
		})
	}

	alerts := make([]sdk.Alert, 0)
	failureCount := len(allRuns)

	if failureCount > 0 {
		s := "s"
		if failureCount == 1 {
			s = ""
		}
		alerts = append(alerts, sdk.Alert{
			ID:       "gha-failures",
			Severity: sdk.SeverityCritical,
			Message:  fmt.Sprintf("%d workflow%s failing across %d repos", failureCount, s, countRepos(allRuns)),
		})
	}

	var severity, value string
	if failureCount > 0 {
		severity = sdk.SeverityCritical
		value = fmt.Sprintf("%d failing", failureCount)
	} else {
		severity = sdk.SeverityInfo
		value = "All passing"
	}

	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateReady,
		Summary:      sdk.Summary{Title: "Actions", Value: value, Trend: sdk.TrendSteady, Severity: severity, IconHint: "workflow"},
		Items:        items,
		Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: 300,
		Health:       sdk.HealthOK,
	}
}

func countRepos(runs []ghWorkflowRun) int {
	seen := make(map[string]bool)
	for _, r := range runs {
		seen[r.Repository.FullName] = true
	}
	return len(seen)
}

func (p *ciPlugin) fetchAllRuns(ctx context.Context, repos []ghRepo) []ghWorkflowRun {
	type result struct {
		runs []ghWorkflowRun
		err  error
	}

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	results := make(chan result, len(repos))

	for _, repo := range repos {
		wg.Add(1)
		go func(r ghRepo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			repoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			runs, err := p.fetchRepoRuns(repoCtx, r.FullName)
			results <- result{runs: runs, err: err}
		}(repo)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allRuns []ghWorkflowRun
	for res := range results {
		if res.err != nil {
			sdk.Log("repo fetch error: %v", res.err)
			continue
		}
		allRuns = append(allRuns, res.runs...)
	}

	return allRuns
}

func (p *ciPlugin) emitDeltaEvents(runs []ghWorkflowRun) {
	p.mu.Lock()
	defer p.mu.Unlock()

	currentFails := make(map[string]prevFailInfo)
	for _, run := range runs {
		key := run.ID.String()
		currentFails[key] = prevFailInfo{title: run.Name, url: run.HTMLURL}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for key, info := range p.prevFails {
		if _, ok := currentFails[key]; !ok && len(p.prevFails) > 0 {
			sdk.Emit(sdk.Event{
				Type:      "workflow.fixed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Workflow %s is passing again", info.title),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"runId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	for key, info := range currentFails {
		if _, ok := p.prevFails[key]; !ok && len(p.prevFails) > 0 {
			sdk.Emit(sdk.Event{
				Type:      "workflow.failed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Workflow failed: %s", info.title),
				Severity:  sdk.SeverityCritical,
				Data:      map[string]string{"runId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	failureCount := len(currentFails)
	if failureCount != p.prevFailureCount && len(p.prevFails) > 0 {
		if failureCount > p.prevFailureCount {
			sdk.Emit(sdk.Event{
				Type:      "failure.count_increased",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Failing workflows increased from %d to %d", p.prevFailureCount, failureCount),
				Severity:  sdk.SeverityCritical,
				Data:      map[string]string{"previous": strconv.Itoa(p.prevFailureCount), "current": strconv.Itoa(failureCount)},
				Timestamp: now,
			})
		} else if failureCount < p.prevFailureCount {
			sdk.Emit(sdk.Event{
				Type:      "failure.count_decreased",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Failing workflows decreased from %d to %d", p.prevFailureCount, failureCount),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"previous": strconv.Itoa(p.prevFailureCount), "current": strconv.Itoa(failureCount)},
				Timestamp: now,
			})
		}
	}

	p.prevFails = currentFails
	p.prevFailureCount = failureCount
}

func main() {
	sdk.Run(pluginID, pluginVersion, &ciPlugin{
		client:    &http.Client{Timeout: 30 * time.Second},
		prevFails: make(map[string]prevFailInfo),
	})
}
