package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID       = "ci-github-actions"
	pluginVersion  = "0.1.1"
	apiBase        = "https://api.github.com"
	maxConcurrency = 8
	maxRepos       = 30
	maxRunsPerRepo = 20 // recent runs scanned to find the latest run per workflow/branch
	maxDisplayRuns = 10

	// runWindow bounds how old a failing run may be to still count. Repos not
	// pushed within the window are skipped, which avoids one request per
	// dormant repository on every refresh.
	runWindow = 14 * 24 * time.Hour

	// refreshBudget keeps a full refresh well inside the host's 30s request timeout.
	refreshBudget  = 25 * time.Second
	perRepoTimeout = 10 * time.Second
	maxBodyBytes   = 4 << 20
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
	WorkflowID json.Number `json:"workflow_id"`
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

// --- HTTP errors ---

type apiError struct {
	StatusCode  int
	RetryAfter  int
	RateLimited bool
}

func (e *apiError) Error() string {
	if e.RateLimited {
		return "GitHub API rate limited"
	}
	return fmt.Sprintf("GitHub API error (HTTP %d)", e.StatusCode)
}

func (e *apiError) health() string {
	if e.RateLimited {
		return httphealth.HealthRateLimited
	}
	return httphealth.ClassifyHTTPStatus(e.StatusCode)
}

// --- Plugin state ---

type prevFailInfo struct {
	runID string
	title string
	url   string
	repo  string
}

type cachedResponse struct {
	etag string
	body []byte
}

type ciPlugin struct {
	token   string
	client  *http.Client
	apiBase string

	mu               sync.Mutex
	prevFails        map[string]prevFailInfo // workflow key -> info
	prevFailureCount int
	seeded           bool

	cacheMu sync.Mutex
	etags   map[string]cachedResponse // path -> last 200 response, for If-None-Match
}

func (p *ciPlugin) Initialize(params sdk.InitializeParams) string {
	p.token = resolveToken(params)
	if p.token == "" {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

// resolveToken picks the GitHub token from initialize params. Provider auth
// records carry the secret in accessToken (OAuth) or apiKey; their accountId is
// a record identifier and is only used as a last resort for older hosts.
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

func (p *ciPlugin) PerformAction(id string, params map[string]string) (bool, string) {
	return false, "unknown action: " + id
}

func (p *ciPlugin) Shutdown() {}

func (p *ciPlugin) apiBaseURL() string {
	if p.apiBase != "" {
		return p.apiBase
	}
	return apiBase
}

// apiRequest performs a conditional GET. GitHub answers 304 for unchanged
// resources, which is faster and does not count against the rate limit.
func (p *ciPlugin) apiRequest(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.apiBaseURL()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "smuler-ci-github-actions-plugin/"+pluginVersion)
	if p.token != "" {
		req.Header.Set("Authorization", "token "+p.token)
	}

	p.cacheMu.Lock()
	cached, hasCached := p.etags[path]
	p.cacheMu.Unlock()
	if hasCached && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusNotModified && hasCached {
		return cached.body, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rateLimited, retryAfter := rateLimitStatus(resp.StatusCode, resp.Header, body)
		return nil, &apiError{StatusCode: resp.StatusCode, RetryAfter: retryAfter, RateLimited: rateLimited}
	}

	if etag := resp.Header.Get("ETag"); etag != "" {
		p.cacheMu.Lock()
		if p.etags == nil {
			p.etags = make(map[string]cachedResponse)
		}
		p.etags[path] = cachedResponse{etag: etag, body: body}
		p.cacheMu.Unlock()
	}
	return body, nil
}

// rateLimitStatus reports whether a GitHub response is a primary or secondary
// rate limit and how many seconds to wait before retrying.
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
	path := fmt.Sprintf("/repos/%s/actions/runs?per_page=%d&exclude_pull_requests=true", fullName, maxRunsPerRepo)
	body, err := p.apiRequest(ctx, path)
	if err != nil {
		return nil, err
	}

	var resp ghRunsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse runs response: %w", err)
	}
	for i := range resp.WorkflowRuns {
		if resp.WorkflowRuns[i].Repository.FullName == "" {
			resp.WorkflowRuns[i].Repository.FullName = fullName
		}
	}
	return resp.WorkflowRuns, nil
}

func errorSnapshot(msg, health string, retryAfter int) sdk.Snapshot {
	severity := sdk.SeverityInfo
	if health == httphealth.HealthAuthReq || health == httphealth.HealthRateLimited {
		severity = sdk.SeverityWarning
	}
	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateDegraded,
		Summary:      sdk.Summary{Title: "GitHub Actions", Value: msg, Trend: sdk.TrendSteady, Severity: severity, IconHint: "workflow"},
		Items:        []sdk.Item{},
		Actions:      []sdk.Action{},
		Alerts:       []sdk.Alert{{ID: "gha-error", Severity: severity, Message: msg}},
		RefreshAfter: httphealth.DefaultRefreshAfter(health, retryAfter),
		Health:       health,
	}
}

func snapshotForError(err error) sdk.Snapshot {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch health := apiErr.health(); health {
		case httphealth.HealthAuthReq:
			return errorSnapshot("GitHub authentication failed — reconnect in Settings", health, apiErr.RetryAfter)
		case httphealth.HealthRateLimited:
			return errorSnapshot("GitHub API rate limited", health, apiErr.RetryAfter)
		default:
			return errorSnapshot(apiErr.Error(), health, apiErr.RetryAfter)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errorSnapshot("GitHub API timed out", httphealth.HealthDegraded, 0)
	}
	return errorSnapshot("GitHub API unreachable", httphealth.HealthDegraded, 0)
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

	ctx, cancel := context.WithTimeout(context.Background(), refreshBudget)
	defer cancel()

	repos, err := p.fetchRepos(ctx)
	if err != nil {
		sdk.Log("repos fetch error: %v", err)
		return snapshotForError(err)
	}

	now := time.Now()
	repos = activeRepos(repos, now)
	fetch := p.fetchAllRuns(ctx, repos)
	if fetch.fatal != nil && len(fetch.okRepos) == 0 && len(repos) > 0 {
		return snapshotForError(fetch.fatal)
	}

	allRuns := failingRuns(fetch.runs, now)
	p.emitDeltaEvents(allRuns, fetch.okRepos)

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

	health := sdk.HealthOK
	refreshAfter := 300
	if fetch.fatal != nil {
		// Some repos could not be checked; surface it instead of claiming "All passing".
		var apiErr *apiError
		health = sdk.HealthDegraded
		if errors.As(fetch.fatal, &apiErr) && apiErr.RateLimited {
			health = sdk.HealthRateLimited
			refreshAfter = httphealth.DefaultRefreshAfter(health, apiErr.RetryAfter)
		}
		alerts = append(alerts, sdk.Alert{
			ID:       "gha-partial",
			Severity: sdk.SeverityWarning,
			Message:  fmt.Sprintf("Could not check %d of %d repos", len(repos)-len(fetch.okRepos), len(repos)),
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
		RefreshAfter: refreshAfter,
		Health:       health,
	}
}

// activeRepos drops repositories with no push inside runWindow.
func activeRepos(repos []ghRepo, now time.Time) []ghRepo {
	out := make([]ghRepo, 0, len(repos))
	for _, r := range repos {
		if pushed, err := time.Parse(time.RFC3339, r.PushedAt); err == nil && now.Sub(pushed) > runWindow {
			continue
		}
		out = append(out, r)
	}
	return out
}

// failingRuns keeps, per repo/workflow/branch, only the most recent completed
// run, and reports it when that run failed. A workflow that failed once and has
// since passed is therefore no longer counted as failing.
func failingRuns(runs []ghWorkflowRun, now time.Time) []ghWorkflowRun {
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].CreatedAt > runs[j].CreatedAt
	})
	seen := make(map[string]bool)
	var failing []ghWorkflowRun
	for _, run := range runs {
		if run.Status != "" && run.Status != "completed" {
			continue
		}
		key := workflowKey(run)
		if seen[key] {
			continue
		}
		seen[key] = true
		switch run.Conclusion {
		case "failure", "timed_out", "startup_failure":
		default:
			continue
		}
		if created, err := time.Parse(time.RFC3339, run.CreatedAt); err == nil && now.Sub(created) > runWindow {
			continue
		}
		failing = append(failing, run)
	}
	return failing
}

func workflowKey(run ghWorkflowRun) string {
	wf := run.WorkflowID.String()
	if wf == "" {
		wf = run.Name
	}
	return run.Repository.FullName + "|" + wf + "|" + run.HeadBranch
}

func countRepos(runs []ghWorkflowRun) int {
	seen := make(map[string]bool)
	for _, r := range runs {
		seen[r.Repository.FullName] = true
	}
	return len(seen)
}

type fetchResult struct {
	runs    []ghWorkflowRun
	okRepos map[string]bool
	fatal   error // first error that means a repo could not be checked
}

func (p *ciPlugin) fetchAllRuns(ctx context.Context, repos []ghRepo) fetchResult {
	type result struct {
		repo string
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
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results <- result{repo: r.FullName, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			repoCtx, cancel := context.WithTimeout(ctx, perRepoTimeout)
			defer cancel()

			runs, err := p.fetchRepoRuns(repoCtx, r.FullName)
			results <- result{repo: r.FullName, runs: runs, err: err}
		}(repo)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	out := fetchResult{okRepos: make(map[string]bool)}
	for res := range results {
		if res.err != nil {
			var apiErr *apiError
			if errors.As(res.err, &apiErr) && !apiErr.RateLimited &&
				(apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden) {
				// Actions disabled or not visible for this repo: nothing to report.
				out.okRepos[res.repo] = true
				continue
			}
			sdk.Log("repo fetch error: %v", res.err)
			if out.fatal == nil || (errors.As(res.err, &apiErr) && apiErr.RateLimited) {
				out.fatal = res.err
			}
			continue
		}
		out.okRepos[res.repo] = true
		out.runs = append(out.runs, res.runs...)
	}

	return out
}

func (p *ciPlugin) emitDeltaEvents(runs []ghWorkflowRun, okRepos map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	currentFails := make(map[string]prevFailInfo)
	for _, run := range runs {
		currentFails[workflowKey(run)] = prevFailInfo{runID: run.ID.String(), title: run.Name, url: run.HTMLURL, repo: run.Repository.FullName}
	}
	// Carry over failures from repos we could not check this time so they are
	// neither reported as fixed now nor as newly failed later.
	for key, info := range p.prevFails {
		if _, ok := currentFails[key]; !ok && !okRepos[info.repo] {
			currentFails[key] = info
		}
	}

	if !p.seeded {
		p.prevFails = currentFails
		p.prevFailureCount = len(currentFails)
		p.seeded = true
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for key, info := range p.prevFails {
		if _, ok := currentFails[key]; !ok {
			sdk.Emit(sdk.Event{
				Type:      "workflow.fixed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Workflow %s is passing again", info.title),
				Severity:  sdk.SeverityInfo,
				Data:      map[string]string{"runId": info.runID, "workflowKey": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	for key, info := range currentFails {
		if _, ok := p.prevFails[key]; !ok {
			sdk.Emit(sdk.Event{
				Type:      "workflow.failed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Workflow failed: %s", info.title),
				Severity:  sdk.SeverityCritical,
				Data:      map[string]string{"runId": info.runID, "workflowKey": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	failureCount := len(currentFails)
	if failureCount != p.prevFailureCount {
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
		client:    &http.Client{Timeout: 15 * time.Second},
		prevFails: make(map[string]prevFailInfo),
		etags:     make(map[string]cachedResponse),
	})
}
