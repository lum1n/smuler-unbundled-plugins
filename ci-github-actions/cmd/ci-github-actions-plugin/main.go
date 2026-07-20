package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/plugindebug"
)

func logDebug(format string, args ...interface{}) {
	plugindebug.Log("[ci-github-actions-plugin]", format, args...)
}

const (
	protocolVersion = "0.1.0"
	pluginVersion   = "0.1.0"
	apiBase         = "https://api.github.com"
	pluginID        = "ci-github-actions"
	maxConcurrency  = 5
	maxRepos        = 30
	maxRunsPerRepo  = 3
	maxDisplayRuns  = 10
)

var initResult = initializedPayload{
	Type:            "initialized",
	ProtocolVersion: protocolVersion,
	PluginVersion:   pluginVersion,
	Health:          "ok",
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
	ProviderID string `json:"providerId"`
	Kind       string `json:"kind"`
	AccountID  string `json:"accountId,omitempty"`
	TokenType  string `json:"tokenType,omitempty"`
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
	HeadSHA   string      `json:"head_sha"`
	Event     string      `json:"event"`
	RunNumber int         `json:"run_number"`
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

func (p *ciPlugin) apiRequest(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", apiBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "smuler-ci-github-actions-plugin/0.1.0")
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

func (p *ciPlugin) buildSnapshot() pluginSnapshot {
	if p.token == "" {
		return pluginSnapshot{
			PluginID: pluginID,
			State:    "degraded",
			Summary:  pluginSummary{Title: "GitHub Actions", Value: "No token configured", Trend: "steady", Severity: "info", IconHint: "workflow"},
			Items:    []pluginItem{},
			Actions:  []pluginAction{},
			Alerts:   []pluginAlert{{ID: "gha-config", Severity: "info", Message: "No token configured"}},
			RefreshAfter: 300,
			Health:   "auth_required",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repos, err := p.fetchRepos(ctx)
	if err != nil {
		msg := fmt.Sprintf("API unreachable: %v", err)
		return pluginSnapshot{
			PluginID: pluginID,
			State:    "degraded",
			Summary:  pluginSummary{Title: "GitHub Actions", Value: msg, Trend: "steady", Severity: "info", IconHint: "workflow"},
			Items:    []pluginItem{},
			Actions:  []pluginAction{},
			Alerts:   []pluginAlert{{ID: "gha-error", Severity: "info", Message: msg}},
			RefreshAfter: 120,
			Health:   "degraded",
		}
	}

	allRuns := p.fetchAllRuns(ctx, repos)
	p.emitDeltaEvents(allRuns)

	sort.Slice(allRuns, func(i, j int) bool {
		return allRuns[i].CreatedAt > allRuns[j].CreatedAt
	})

	items := make([]pluginItem, 0, maxDisplayRuns)
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

		items = append(items, pluginItem{
			ID:        runID,
			Title:     run.Name,
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  "critical",
			Timestamp: run.CreatedAt,
			DeepLink:  run.HTMLURL,
			Actions:   []pluginAction{{ID: "open", Label: "Open Run"}},
		})
	}

	alerts := make([]pluginAlert, 0)
	failureCount := len(allRuns)

	if failureCount > 0 {
		s := "s"
		if failureCount == 1 {
			s = ""
		}
		alerts = append(alerts, pluginAlert{
			ID:       "gha-failures",
			Severity: "critical",
			Message:  fmt.Sprintf("%d workflow%s failing across %d repos", failureCount, s, countRepos(allRuns)),
		})
	}

	var severity string
	var value string
	var health string
	if failureCount > 0 {
		severity = "critical"
		value = fmt.Sprintf("%d failing", failureCount)
		health = "ok"
	} else {
		severity = "info"
		value = "All passing"
		health = "ok"
	}

	return pluginSnapshot{
		PluginID:     pluginID,
		State:        "ready",
		Summary:      pluginSummary{Title: "Actions", Value: value, Trend: "steady", Severity: severity, IconHint: "workflow"},
		Items:        items,
		Actions:      []pluginAction{{ID: "refresh", Label: "Refresh"}},
		Alerts:       alerts,
		RefreshAfter: 300,
		Health:       health,
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
			logDebug("repo fetch error: %v", res.err)
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
			p.emitEvent(pluginEvent{
				Type:      "workflow.fixed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Workflow %s is passing again", info.title),
				Severity:  "info",
				Data:      map[string]string{"runId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	for key, info := range currentFails {
		if _, ok := p.prevFails[key]; !ok && len(p.prevFails) > 0 {
			p.emitEvent(pluginEvent{
				Type:      "workflow.failed",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Workflow failed: %s", info.title),
				Severity:  "critical",
				Data:      map[string]string{"runId": key, "title": info.title, "url": info.url},
				Timestamp: now,
			})
		}
	}

	failureCount := len(currentFails)
	if failureCount != p.prevFailureCount && len(p.prevFails) > 0 {
		if failureCount > p.prevFailureCount {
			p.emitEvent(pluginEvent{
				Type:      "failure.count_increased",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Failing workflows increased from %d to %d", p.prevFailureCount, failureCount),
				Severity:  "critical",
				Data:      map[string]string{"previous": strconv.Itoa(p.prevFailureCount), "current": strconv.Itoa(failureCount)},
				Timestamp: now,
			})
		} else if failureCount < p.prevFailureCount {
			p.emitEvent(pluginEvent{
				Type:      "failure.count_decreased",
				PluginID:  pluginID,
				Message:   fmt.Sprintf("Failing workflows decreased from %d to %d", p.prevFailureCount, failureCount),
				Severity:  "info",
				Data:      map[string]string{"previous": strconv.Itoa(p.prevFailureCount), "current": strconv.Itoa(failureCount)},
				Timestamp: now,
			})
		}
	}

	p.prevFails = currentFails
	p.prevFailureCount = failureCount
}

func (p *ciPlugin) emitEvent(event pluginEvent) {
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

func main() {
	defer func() {
		if r := recover(); r != nil {
			logDebug("panic: %v", r)
		}
	}()

	pl := &ciPlugin{
		client:    &http.Client{Timeout: 30 * time.Second},
		prevFails: make(map[string]prevFailInfo),
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

		default:
			sendError(req.ID, -32601, fmt.Sprintf("unknown method: %s", req.Method), false, "")
		}
	}

	if err := scanner.Err(); err != nil {
		logDebug("stdin scanner error: %v", err)
	}
}
