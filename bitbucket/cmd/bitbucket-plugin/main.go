package main

import (
	"bufio"
	"encoding/base64"
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

	"github.com/lum1n/smuler/plugins/httphealth"
	"github.com/lum1n/smuler/plugins/plugindebug"
)

func logDebug(format string, args ...interface{}) {
	plugindebug.Log("[bitbucket-plugin]", format, args...)
}

const (
	protocolVersion = "0.1.0"
	pluginVersion   = "0.1.2"
	cloudAPIBase    = "https://api.bitbucket.org/2.0"
	cloudWebBase    = "https://bitbucket.org"
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
	AccountID    string `json:"accountId,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
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
	Success bool            `json:"success"`
	Data    string          `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
	Window  *pluginWindow   `json:"window,omitempty"`
	AI      *pluginAIAssist `json:"ai,omitempty"`
}

type pluginWindow struct {
	ID       string                `json:"id"`
	Title    string                `json:"title"`
	Subtitle string                `json:"subtitle,omitempty"`
	IconHint string                `json:"iconHint,omitempty"`
	Sections []pluginWindowSection `json:"sections"`
}

type pluginWindowSection struct {
	ID     string              `json:"id"`
	Title  string              `json:"title,omitempty"`
	Blocks []pluginWindowBlock `json:"blocks"`
}

type pluginWindowBlock struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Style string `json:"style,omitempty"`
}

type pluginAIAssist struct {
	Task         string `json:"task"`
	Input        string `json:"input"`
	WindowID     string `json:"windowId"`
	SectionTitle string `json:"sectionTitle,omitempty"`
}

// --- Common API types (normalized; both Cloud and Server map into these) ---

type bbUser struct {
	UUID        string
	Username    string
	DisplayName string
}

type bbRepo struct {
	FullName string
	Slug     string
	Name     string
}

type bbPR struct {
	ID           int
	Title        string
	State        string
	Author       bbUser
	CreatedOn    string
	UpdatedOn    string
	SourceBranch string
	DestBranch   string
	SourceCommit string
	Reviewers    []bbUser
	Participants []bbParticipant
	DeepLink     string
	Summary      string
	RepoSlug     string
}

type bbParticipant struct {
	User     bbUser
	Approved bool
	State    string
}

type bbCommitStatus struct {
	Key       string
	Name      string
	State     string
	URL       string
	CreatedOn string
}

// --- Cloud API types ---

type cloudUser struct {
	UUID        string `json:"uuid"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

type cloudRepo struct {
	FullName string `json:"full_name"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
}

type cloudPaginatedRepos struct {
	Values []cloudRepo `json:"values"`
	Next   string      `json:"next"`
}

type cloudPR struct {
	ID           int                `json:"id"`
	Title        string             `json:"title"`
	State        string             `json:"state"`
	Author       cloudUser          `json:"author"`
	CreatedOn    string             `json:"created_on"`
	UpdatedOn    string             `json:"updated_on"`
	Source       cloudRef           `json:"source"`
	Destination  cloudRef           `json:"destination"`
	Reviewers    []cloudUser        `json:"reviewers"`
	Participants []cloudParticipant `json:"participants"`
	Links        struct {
		HTML struct {
			HREF string `json:"href"`
		} `json:"html"`
	} `json:"links"`
	Summary struct {
		Raw string `json:"raw"`
	} `json:"summary"`
}

type cloudRef struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
	Commit struct {
		Hash string `json:"hash"`
	} `json:"commit"`
}

type cloudParticipant struct {
	User     cloudUser `json:"user"`
	Approved bool      `json:"approved"`
	State    string    `json:"state"`
}

type cloudPaginatedPRs struct {
	Values []cloudPR `json:"values"`
	Next   string    `json:"next"`
}

type cloudPaginatedPRComments struct {
	Values []cloudPRComment `json:"values"`
}

type cloudPRComment struct {
	User    cloudUser `json:"user"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
}

type cloudDiffStat struct {
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
}

type cloudPaginatedDiffStats struct {
	Values []cloudDiffStat `json:"values"`
}

type serverPRActivity struct {
	Action  string   `json:"action"`
	User    serverUser `json:"user"`
	Comment struct {
		Text string `json:"text"`
	} `json:"comment"`
}

type cloudStatus struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	State     string `json:"state"`
	URL       string `json:"url"`
	CreatedOn string `json:"created_on"`
}

type cloudPaginatedStatuses struct {
	Values []cloudStatus `json:"values"`
}

// --- Server API types ---

type serverUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type serverPaginatedResponse struct {
	Values       []json.RawMessage `json:"values"`
	Size         int               `json:"size"`
	IsLastPage   bool              `json:"isLastPage"`
	NextPageStart *int             `json:"nextPageStart"`
}

type serverRepo struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Links struct {
		Clone []struct {
			HREF string `json:"href"`
			Name string `json:"name"`
		} `json:"clone"`
	} `json:"links"`
}

type serverPR struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	CreatedDate int64  `json:"createdDate"`
	UpdatedDate int64  `json:"updatedDate"`
	Author      struct {
		User serverUser `json:"user"`
	} `json:"author"`
	FromRef struct {
		DisplayID   string `json:"displayId"`
		LatestCommit string `json:"latestCommit"`
	} `json:"fromRef"`
	ToRef struct {
		DisplayID string `json:"displayId"`
	} `json:"toRef"`
	Reviewers []struct {
		User serverUser `json:"user"`
	} `json:"reviewers"`
	Participants []serverParticipant `json:"participants"`
	Links        struct {
		Self []struct {
			HREF string `json:"href"`
		} `json:"self"`
	} `json:"links"`
	Description string `json:"description"`
}

type serverParticipant struct {
	User     serverUser `json:"user"`
	Role     string     `json:"role"`
	Approved bool       `json:"approved"`
	Status   string     `json:"status"`
}

type serverBuild struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	State     string `json:"state"`
	URL       string `json:"url"`
	BuildDate int64  `json:"buildDate"`
}

// --- Config ---

type bitbucketConfig struct {
	ServerURL          string
	Username           string
	Workspace          string
	ShowMyPRs          bool
	ShowReviewRequests bool
	ShowPipelines      bool
	MaxPRs             int
	ExcludedUsers      []string // usernames to exclude from results
}

// --- Auth ---

type bitbucketAuth struct {
	Kind         string
	APIKey       string
	AccessToken  string
	CookieHeader string
}

func (a bitbucketAuth) hasAuth() bool {
	return a.APIKey != "" || a.AccessToken != "" || a.CookieHeader != ""
}

func (a bitbucketAuth) apply(req *http.Request) {
	switch {
	case a.APIKey != "":
		parts := strings.SplitN(a.APIKey, ":", 2)
		user := ""
		pass := ""
		if len(parts) == 2 {
			user = parts[0]
			pass = parts[1]
		} else {
			user = parts[0]
		}
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	case a.AccessToken != "":
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	case a.CookieHeader != "":
		req.Header.Set("Cookie", a.CookieHeader)
	}
}

func (a bitbucketAuth) username() string {
	if a.Kind == "api_key" {
		parts := strings.SplitN(a.APIKey, ":", 2)
		if len(parts) == 2 {
			return parts[0]
		}
		return parts[0]
	}
	return ""
}

// --- Plugin ---

type prStateInfo struct {
	hash     string
	deepLink string
}

type bitbucketPlugin struct {
	client       *http.Client
	config       bitbucketConfig
	auth         bitbucketAuth
	apiBase      string
	webBase      string
	serverMode   bool
	workspace    string
	currentUser  *bbUser
	prevPRStates map[string]prStateInfo
	prCache      map[int]bbPR // PR ID -> metadata for getMRDiff lookups
}

func (p *bitbucketPlugin) resolveBases() {
	if p.config.ServerURL != "" {
		p.apiBase = p.config.ServerURL + "/rest/api/1.0"
		p.webBase = p.config.ServerURL
		p.serverMode = true
	} else {
		p.apiBase = cloudAPIBase
		p.webBase = cloudWebBase
		p.serverMode = false
	}
}

func main() {
	logDebug("starting bitbucket plugin")

	pl := &bitbucketPlugin{
		client: &http.Client{Timeout: 25 * time.Second},
		config: bitbucketConfig{
			ShowMyPRs:         true,
			ShowReviewRequests: true,
			MaxPRs:            10,
		},
		prevPRStates: make(map[string]prStateInfo),
		prCache:      make(map[int]bbPR),
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
				sendError(req.ID, -32000, err.Error(), false, "Check Bitbucket workspace and credentials.")
			}
		case "getStatus", "refresh":
			snap, err := pl.buildSnapshot()
			if err != nil {
				logDebug("buildSnapshot failed: %v", err)
				sendError(req.ID, -32000, err.Error(), true, "Bitbucket API may be unreachable.")
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

func (p *bitbucketPlugin) handleInitialize(req rpcRequest) error {
	var params initializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fmt.Errorf("decode initialize: %w", err)
	}

	plugindebug.ConfigureFromInitializeConfig(params.Config)
	p.config = parseConfig(params.Config)
	p.workspace = strings.TrimSpace(p.config.Workspace)
	p.resolveBases()

	for _, pa := range params.ProviderAuths {
		switch pa.Kind {
		case "api_key":
			if pa.APIKey != "" {
				p.auth.Kind = "api_key"
				p.auth.APIKey = pa.APIKey
			}
		case "oauth":
			if pa.AccessToken != "" {
				p.auth.Kind = "oauth"
				p.auth.AccessToken = pa.AccessToken
			}
		case "browser_import":
			if pa.CookieHeader != "" {
				p.auth.Kind = "browser_import"
				p.auth.CookieHeader = pa.CookieHeader
			}
		}
	}

	// In server mode, derive username from config or from app password
	if p.serverMode && p.config.Username == "" {
		if u := p.auth.username(); u != "" {
			p.config.Username = u
		}
	}

	logDebug("initialize workspace=%q authKind=%s serverMode=%t username=%q showMyPRs=%t showReviewRequests=%t showPipelines=%t max=%d",
		p.workspace, p.auth.Kind, p.serverMode, p.config.Username, p.config.ShowMyPRs, p.config.ShowReviewRequests,
		p.config.ShowPipelines, p.config.MaxPRs)

	sendResult(req.ID, initializedPayload{
		Type:            "initialized",
		ProtocolVersion: protocolVersion,
		PluginVersion:   pluginVersion,
		Health:          "ok",
	})
	return nil
}

func (p *bitbucketPlugin) handlePerformAction(req rpcRequest) (*performActionResult, error) {
	if p.workspace == "" {
		return nil, fmt.Errorf("no Bitbucket workspace configured")
	}
	if !p.auth.hasAuth() {
		return nil, fmt.Errorf("no Bitbucket auth configured")
	}

	var params performActionParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("decode performAction params: %w", err)
	}

	switch params.ActionID {
	case "getPRDetails":
		idStr := strings.TrimSpace(params.Payload["id"])
		repoSlug := strings.TrimSpace(params.Payload["repoSlug"])
		if idStr == "" || repoSlug == "" {
			return nil, fmt.Errorf("missing id or repoSlug payload")
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return nil, fmt.Errorf("invalid id payload")
		}
		return p.getPRDetails(id, repoSlug)
	case "getMRDiff":
		idStr := strings.TrimSpace(params.Payload["id"])
		if idStr == "" {
			return nil, fmt.Errorf("missing id payload")
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return nil, fmt.Errorf("invalid id payload")
		}
		return p.getMRDiff(id)
	case "summarize":
		idStr := strings.TrimSpace(params.Payload["id"])
		if idStr == "" {
			return nil, fmt.Errorf("missing id payload")
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			return nil, fmt.Errorf("invalid id payload")
		}
		return p.summarizePR(id)
	case "searchPRs":
		query := strings.TrimSpace(params.Payload["query"])
		if query == "" {
			return nil, fmt.Errorf("missing query payload")
		}
		return p.searchPRs(query)
	default:
		return nil, fmt.Errorf("unknown action %q", params.ActionID)
	}
}

func (p *bitbucketPlugin) getPRDetails(id int, repoSlug string) (*performActionResult, error) {
	if p.serverMode {
		return p.getPRDetailsServer(id, repoSlug)
	}
	return p.getPRDetailsCloud(id, repoSlug)
}

func (p *bitbucketPlugin) getPRDetailsCloud(id int, repoSlug string) (*performActionResult, error) {
	base := fmt.Sprintf("%s/repositories/%s/%s/pullrequests/%d", p.apiBase, p.workspace, repoSlug, id)

	var pr cloudPR
	if err := p.getJSON(base, &pr); err != nil {
		return nil, err
	}
	mapped := p.mapCloudPR(pr, repoSlug)

	var comments []string
	var commentPage cloudPaginatedPRComments
	if err := p.getJSON(base+"/comments?pagelen=50", &commentPage); err == nil {
		for _, c := range commentPage.Values {
			if text := strings.TrimSpace(c.Content.Raw); text != "" {
				comments = append(comments, fmt.Sprintf("%s: %s", c.User.DisplayName, truncate(text, 200)))
			}
		}
	}

	var diffStat []string
	var diffPage cloudPaginatedDiffStats
	if err := p.getJSON(base+"/diffstat?pagelen=50", &diffPage); err == nil {
		added, removed := 0, 0
		for _, d := range diffPage.Values {
			added += d.LinesAdded
			removed += d.LinesRemoved
		}
		diffStat = append(diffStat, fmt.Sprintf("%d files changed, +%d -%d", len(diffPage.Values), added, removed))
	}

	return p.formatPRDetails(mapped, comments, diffStat), nil
}

func (p *bitbucketPlugin) getPRDetailsServer(id int, repoSlug string) (*performActionResult, error) {
	base := fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d", p.apiBase, p.workspace, repoSlug, id)

	var sp serverPR
	if err := p.getJSON(base, &sp); err != nil {
		return nil, err
	}
	repo := bbRepo{Slug: repoSlug, FullName: p.workspace + "/" + repoSlug}
	mapped := p.mapServerPR(sp, repo)

	var comments []string
	var commentPage serverPaginatedResponse
	if err := p.getJSON(base+"/activities?limit=50", &commentPage); err == nil {
		for _, raw := range commentPage.Values {
			var activity serverPRActivity
			if err := json.Unmarshal(raw, &activity); err != nil {
				continue
			}
			if activity.Action == "COMMENTED" && strings.TrimSpace(activity.Comment.Text) != "" {
				comments = append(comments, fmt.Sprintf("%s: %s", activity.User.DisplayName, truncate(activity.Comment.Text, 200)))
			}
		}
	}

	diffStat := []string{"(diff stats not available for server mode)"}

	return p.formatPRDetails(mapped, comments, diffStat), nil
}

func (p *bitbucketPlugin) formatPRDetails(pr bbPR, comments, diffStat []string) *performActionResult {
	approvals := countApprovals(pr)
	changesRequested := countChangesRequested(pr)

	var lines []string
	lines = append(lines, fmt.Sprintf("PR #%d: %s", pr.ID, pr.Title))
	lines = append(lines, fmt.Sprintf("Author: %s | Branch: %s → %s", pr.Author.DisplayName, pr.SourceBranch, pr.DestBranch))
	lines = append(lines, fmt.Sprintf("Approvals: %d | Changes requested: %d", approvals, changesRequested))
	if len(diffStat) > 0 {
		lines = append(lines, "Diff: "+diffStat[0])
	}
	description := strings.TrimSpace(pr.Summary)
	if description == "" {
		description = "(no description)"
	}
	lines = append(lines, fmt.Sprintf("Description: %s", description))
	if len(comments) > 0 {
		lines = append(lines, "Comments:")
		for _, c := range comments {
			lines = append(lines, "  - "+c)
		}
	}
	lines = append(lines, fmt.Sprintf("Link: %s", pr.DeepLink))

	return &performActionResult{Success: true, Data: strings.Join(lines, "\n")}
}

func (p *bitbucketPlugin) getMRDiff(id int) (*performActionResult, error) {
	pr, ok := p.prCache[id]
	if !ok {
		return nil, fmt.Errorf("PR %d not found in cache", id)
	}

	var url string
	if p.serverMode {
		url = fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d/diff", p.apiBase, p.workspace, pr.RepoSlug, id)
	} else {
		url = fmt.Sprintf("%s/repositories/%s/%s/pullrequests/%d/diff", p.apiBase, p.workspace, pr.RepoSlug, id)
	}

	diff, err := p.fetchRaw(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch diff: %w", err)
	}
	return &performActionResult{Success: true, Data: diff}, nil
}

func (p *bitbucketPlugin) summarizePR(id int) (*performActionResult, error) {
	pr, ok := p.prCache[id]
	if !ok {
		return nil, fmt.Errorf("PR %d not found in cache", id)
	}

	diffResult, err := p.getMRDiff(id)
	if err != nil {
		return nil, err
	}
	if diffResult == nil || strings.TrimSpace(diffResult.Data) == "" {
		return &performActionResult{Success: false, Error: "Empty diff"}, nil
	}

	title := pr.Title
	if title == "" {
		title = pr.Summary
	}
	if title == "" {
		title = fmt.Sprintf("PR #%d", id)
	}
	windowID := fmt.Sprintf("bitbucket.summarize.%d", id)
	return &performActionResult{
		Success: true,
		Window: &pluginWindow{
			ID:       windowID,
			Title:    title,
			Subtitle: "Bitbucket",
			IconHint: "arrow.triangle.pull",
			Sections: []pluginWindowSection{{
				ID:    "loading",
				Title: "Summary",
				Blocks: []pluginWindowBlock{{
					ID:    "generating",
					Text:  "Generating summary...",
					Style: "paragraph",
				}},
			}},
		},
		AI: &pluginAIAssist{
			Task:         "summarize_bullets",
			Input:        diffResult.Data,
			WindowID:     windowID,
			SectionTitle: "Summary",
		},
	}, nil
}

func (p *bitbucketPlugin) fetchRaw(urlStr string) (string, error) {
	resp, err := p.doAPI("GET", urlStr)
	if err != nil {
		return "", fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

func (p *bitbucketPlugin) searchPRs(query string) (*performActionResult, error) {
	repos, err := p.fetchRepos()
	if err != nil {
		return nil, err
	}

	var all []bbPR
	if p.serverMode {
		all, err = p.fetchAllPRsServer(repos)
	} else {
		all, err = p.fetchAllPRsCloud(repos)
	}
	if err != nil && !strings.HasPrefix(err.Error(), "partial:") {
		return nil, err
	}

	queryLower := strings.ToLower(query)
	var matches []bbPR
	for _, pr := range all {
		if strings.Contains(strings.ToLower(pr.Title), queryLower) ||
			strings.Contains(strings.ToLower(pr.Author.DisplayName), queryLower) ||
			strings.Contains(strings.ToLower(pr.SourceBranch), queryLower) ||
			strings.Contains(strings.ToLower(pr.DestBranch), queryLower) {
			matches = append(matches, pr)
		}
	}

	if len(matches) == 0 {
		return &performActionResult{Success: true, Data: "No matching pull requests found."}, nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d matching PR(s):", len(matches)))
	for _, pr := range matches {
		description := strings.TrimSpace(pr.Summary)
		line := fmt.Sprintf("#%d %s | %s → %s | %s", pr.ID, pr.Title, pr.SourceBranch, pr.DestBranch, pr.Author.DisplayName)
		if description != "" {
			line += " | " + truncate(description, 120)
		}
		lines = append(lines, "  - "+line)
	}

	return &performActionResult{Success: true, Data: strings.Join(lines, "\n")}, nil
}

func (p *bitbucketPlugin) buildSnapshot() (*pluginSnapshot, error) {
	if p.workspace == "" {
		return &pluginSnapshot{
			PluginID:     "bitbucket",
			State:        "degraded",
			Summary:      pluginSummary{Title: "Bitbucket", Value: "No workspace", Trend: "steady", Severity: "info", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{},
			RefreshAfter: 120,
			Health:       "degraded",
		}, nil
	}

	if !p.auth.hasAuth() {
		return &pluginSnapshot{
			PluginID:     "bitbucket",
			State:        "error",
			Summary:      pluginSummary{Title: "Bitbucket", Value: "No auth", Trend: "steady", Severity: "warning", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "bitbucket-no-auth", Severity: "warning", Message: "Connect a Bitbucket account via Settings to see pull requests."}},
			RefreshAfter: 120,
			Health:       "auth_required",
		}, nil
	}

	user, err := p.fetchCurrentUser()
	if err != nil {
		logDebug("fetchCurrentUser error: %v", err)
		return p.snapshotFromAPIError("Auth err", err), nil
	}
	p.currentUser = user

	if user.Username == "" {
		return &pluginSnapshot{
			PluginID:     "bitbucket",
			State:        "degraded",
			Summary:      pluginSummary{Title: "Bitbucket", Value: "No username", Trend: "steady", Severity: "warning", IconHint: "pull-request"},
			Items:        []pluginItem{},
			Actions:      []pluginAction{},
			Alerts:       []pluginAlert{{ID: "bitbucket-no-username", Severity: "warning", Message: "Set your Bitbucket username in plugin settings to filter pull requests."}},
			RefreshAfter: 120,
			Health:       "degraded",
		}, nil
	}

	repos, err := p.fetchRepos()
	if err != nil {
		logDebug("fetchRepos error: %v", err)
		return p.snapshotFromAPIError("API err", err), nil
	}

	allPRs, err := p.fetchAllPRs(repos)
	var partialFetchErrors string
	if err != nil {
		logDebug("fetchAllPRs error: %v", err)
		if strings.HasPrefix(err.Error(), "partial:") {
			partialFetchErrors = strings.TrimPrefix(err.Error(), "partial: ")
		} else {
			return p.snapshotFromAPIError("API err", err), nil
		}
	}

	filtered := p.filterPRs(allPRs)
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].UpdatedOn > filtered[j].UpdatedOn
	})
	if len(filtered) > p.config.MaxPRs {
		filtered = filtered[:p.config.MaxPRs]
	}

	prStatuses := make(map[int][]bbCommitStatus)
	if p.config.ShowPipelines {
		prStatuses = p.fetchBuildStatuses(filtered)
	}

	items := p.buildItems(filtered, prStatuses)
	alerts := p.buildAlerts(filtered, prStatuses)
	summary := p.buildSummary(items, len(allPRs))

	// Rebuild PR cache for getMRDiff lookups
	p.prCache = make(map[int]bbPR)
	for _, pr := range filtered {
		p.prCache[pr.ID] = pr
	}

	if partialFetchErrors != "" {
		alerts = append([]pluginAlert{{ID: "bitbucket-partial-fetch", Severity: "warning", Message: "Some repositories could not be fetched: " + partialFetchErrors}}, alerts...)
	}

	if items == nil {
		items = []pluginItem{}
	}
	if alerts == nil {
		alerts = []pluginAlert{}
	}

	p.detectEvents(filtered, prStatuses)

	return &pluginSnapshot{
		PluginID:     "bitbucket",
		State:        "ready",
		Summary:      summary,
		Items:        items,
		Actions:      []pluginAction{},
		Alerts:       alerts,
		RefreshAfter: 120,
		Health:       "ok",
	}, nil
}

func (p *bitbucketPlugin) degradedSnapshot(value, message, health string, retryAfter int) *pluginSnapshot {
	if health == "" {
		health = httphealth.HealthDegraded
	}
	return &pluginSnapshot{
		PluginID:     "bitbucket",
		State:        "degraded",
		Summary:      pluginSummary{Title: "Bitbucket", Value: value, Trend: "steady", Severity: "warning", IconHint: "pull-request"},
		Items:        []pluginItem{},
		Actions:      []pluginAction{},
		Alerts:       []pluginAlert{{ID: "bitbucket-api-err", Severity: "warning", Message: message}},
		RefreshAfter: httphealth.DefaultRefreshAfter(health, retryAfter),
		Health:       health,
	}
}

func (p *bitbucketPlugin) snapshotFromAPIError(value string, err error) *pluginSnapshot {
	if apiErr, ok := err.(*apiHTTPError); ok {
		health := httphealth.ClassifyHTTPStatus(apiErr.StatusCode)
		message := "Could not reach Bitbucket: " + apiErr.Error()
		if health == httphealth.HealthAuthReq {
			message = "Bitbucket authentication failed — reconnect in Settings"
		} else if health == httphealth.HealthRateLimited {
			message = "Bitbucket API rate limited"
		}
		return p.degradedSnapshot(value, message, health, apiErr.RetryAfter)
	}
	return p.degradedSnapshot(value, "Could not reach Bitbucket: "+err.Error(), httphealth.HealthDegraded, 0)
}

// --- HTTP helpers ---

func (p *bitbucketPlugin) doAPI(method, urlStr string) (*http.Response, error) {
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "smuler-bitbucket-plugin/0.1.2")
	p.auth.apply(req)
	return p.client.Do(req)
}

func (p *bitbucketPlugin) doAPIGetHeaders(urlStr string) (*http.Response, http.Header, error) {
	resp, err := p.doAPI("GET", urlStr)
	if err != nil {
		return nil, nil, err
	}
	headers := make(http.Header)
	for k, v := range resp.Header {
		headers[k] = v
	}
	return resp, headers, nil
}

func (p *bitbucketPlugin) getJSON(urlStr string, target interface{}) error {
	resp, err := p.doAPI("GET", urlStr)
	if err != nil {
		return fmt.Errorf("api request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       string(body),
		}
	}

	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode response: %w (%s)", err, string(body))
	}
	return nil
}

// --- fetchCurrentUser ---

func (p *bitbucketPlugin) fetchCurrentUser() (*bbUser, error) {
	if p.serverMode {
		return p.fetchCurrentUserServer()
	}
	return p.fetchCurrentUserCloud()
}

func (p *bitbucketPlugin) fetchCurrentUserCloud() (*bbUser, error) {
	var cu cloudUser
	if err := p.getJSON(p.apiBase+"/user", &cu); err != nil {
		return nil, err
	}
	logDebug("current user (cloud) uuid=%s username=%s", cu.UUID, cu.Username)
	return &bbUser{UUID: cu.UUID, Username: cu.Username, DisplayName: cu.DisplayName}, nil
}

func (p *bitbucketPlugin) fetchCurrentUserServer() (*bbUser, error) {
	username := p.config.Username

	// If we have a username from config/app-password, try to look up the user
	if username != "" {
		url := fmt.Sprintf("%s/users/%s", p.apiBase, username)
		var su serverUser
		if err := p.getJSON(url, &su); err != nil {
			logDebug("server user lookup failed for %q: %v", username, err)
		} else {
			logDebug("current user (server) name=%s displayName=%s", su.Name, su.DisplayName)
			return &bbUser{UUID: su.Name, Username: su.Name, DisplayName: su.DisplayName}, nil
		}
	}

	// Try X-Ausername header from a lightweight request
	resp, headers, err := p.doAPIGetHeaders(p.apiBase + "/application-properties")
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 400 {
		resp.Body.Close()
		if h := headers.Get("X-Ausername"); h != "" {
			logDebug("current user (server, from header) username=%s", h)
			return &bbUser{UUID: h, Username: h, DisplayName: h}, nil
		}
	}
	if resp != nil {
		resp.Body.Close()
	}

	return nil, fmt.Errorf("could not determine Bitbucket username — set it in plugin settings")
}

// --- fetchRepos ---

func (p *bitbucketPlugin) fetchRepos() ([]bbRepo, error) {
	if p.serverMode {
		return p.fetchReposServer()
	}
	return p.fetchReposCloud()
}

func (p *bitbucketPlugin) fetchReposCloud() ([]bbRepo, error) {
	var all []bbRepo
	nextURL := fmt.Sprintf("%s/repositories/%s?role=member&pagelen=100&fields=values.slug,values.full_name,values.name,next", p.apiBase, p.workspace)

	for nextURL != "" {
		var page cloudPaginatedRepos
		if err := p.getJSON(nextURL, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Values {
			all = append(all, bbRepo{FullName: r.FullName, Slug: r.Slug, Name: r.Name})
		}
		nextURL = page.Next
	}

	logDebug("fetched repos count=%d (cloud)", len(all))
	return all, nil
}

func (p *bitbucketPlugin) fetchReposServer() ([]bbRepo, error) {
	var all []bbRepo
	start := 0

	for {
		url := fmt.Sprintf("%s/projects/%s/repos?limit=100&start=%d", p.apiBase, p.workspace, start)
		var page serverPaginatedResponse
		if err := p.getJSON(url, &page); err != nil {
			return nil, err
		}
		for _, raw := range page.Values {
			var r serverRepo
			if err := json.Unmarshal(raw, &r); err != nil {
				continue
			}
			fullName := p.workspace + "/" + r.Slug
			all = append(all, bbRepo{FullName: fullName, Slug: r.Slug, Name: r.Name})
		}
		if page.IsLastPage {
			break
		}
		if page.NextPageStart != nil {
			start = *page.NextPageStart
		} else {
			start += len(page.Values)
		}
	}

	logDebug("fetched repos count=%d (server)", len(all))
	return all, nil
}

// --- fetchAllPRs ---

func (p *bitbucketPlugin) fetchAllPRs(repos []bbRepo) ([]bbPR, error) {
	if p.serverMode {
		return p.fetchAllPRsServer(repos)
	}
	return p.fetchAllPRsCloud(repos)
}

func (p *bitbucketPlugin) fetchAllPRsCloud(repos []bbRepo) ([]bbPR, error) {
	var all []bbPR
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, len(repos))

	prFields := "values.id,values.title,values.state,values.author,values.created_on,values.updated_on," +
		"values.source.branch.name,values.source.commit.hash," +
		"values.destination.branch.name," +
		"values.reviewers,values.participants,values.links.html.href,values.summary.raw,next"

	for _, repo := range repos {
		wg.Add(1)
		go func(r bbRepo) {
			defer wg.Done()
			nextURL := fmt.Sprintf("%s/repositories/%s/%s/pullrequests?state=OPEN&pagelen=30&fields=%s",
				p.apiBase, p.workspace, r.Slug, prFields)

			for nextURL != "" {
				var page cloudPaginatedPRs
				if err := p.getJSON(nextURL, &page); err != nil {
					errCh <- fmt.Errorf("%s: %w", r.FullName, err)
					return
				}
			mu.Lock()
			for _, pr := range page.Values {
				all = append(all, p.mapCloudPR(pr, r.Slug))
			}
			mu.Unlock()
				nextURL = page.Next
			}
		}(repo)
	}
	wg.Wait()
	close(errCh)

	var errs []string
	for e := range errCh {
		errs = append(errs, e.Error())
	}
	if len(errs) > 0 {
		logDebug("some repo fetches failed: %s", strings.Join(errs, "; "))
		return all, fmt.Errorf("partial: %s", strings.Join(errs, "; "))
	}

	logDebug("fetched prs count=%d (cloud)", len(all))
	return all, nil
}

func (p *bitbucketPlugin) mapCloudPR(pr cloudPR, repoSlug string) bbPR {
	mapped := bbPR{
		ID:           pr.ID,
		Title:        pr.Title,
		State:        pr.State,
		CreatedOn:    pr.CreatedOn,
		UpdatedOn:    pr.UpdatedOn,
		SourceBranch: pr.Source.Branch.Name,
		DestBranch:   pr.Destination.Branch.Name,
		SourceCommit: pr.Source.Commit.Hash,
		DeepLink:     pr.Links.HTML.HREF,
		Summary:      pr.Summary.Raw,
		RepoSlug:     repoSlug,
	}
	if pr.Title == "" {
		mapped.Title = pr.Summary.Raw
	}
	mapped.Author = bbUser{UUID: pr.Author.UUID, Username: pr.Author.Username, DisplayName: pr.Author.DisplayName}
	for _, r := range pr.Reviewers {
		mapped.Reviewers = append(mapped.Reviewers, bbUser{UUID: r.UUID, Username: r.Username, DisplayName: r.DisplayName})
	}
	for _, p := range pr.Participants {
		mapped.Participants = append(mapped.Participants, bbParticipant{
			User:     bbUser{UUID: p.User.UUID, Username: p.User.Username, DisplayName: p.User.DisplayName},
			Approved: p.Approved,
			State:    p.State,
		})
	}
	return mapped
}

func (p *bitbucketPlugin) fetchAllPRsServer(repos []bbRepo) ([]bbPR, error) {
	var all []bbPR
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, len(repos))

	for _, repo := range repos {
		wg.Add(1)
		go func(r bbRepo) {
			defer wg.Done()
			start := 0
			for {
				url := fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests?state=OPEN&limit=30&start=%d",
					p.apiBase, p.workspace, r.Slug, start)

				var page serverPaginatedResponse
				if err := p.getJSON(url, &page); err != nil {
					errCh <- fmt.Errorf("%s: %w", r.FullName, err)
					return
				}

				mu.Lock()
				for _, raw := range page.Values {
					var sp serverPR
					if err := json.Unmarshal(raw, &sp); err != nil {
						logDebug("unmarshal server pr: %v", err)
						continue
					}
					all = append(all, p.mapServerPR(sp, r))
				}
				mu.Unlock()

				if page.IsLastPage {
					break
				}
				if page.NextPageStart != nil {
					start = *page.NextPageStart
				} else {
					start += len(page.Values)
				}
			}
		}(repo)
	}
	wg.Wait()
	close(errCh)

	var errs []string
	for e := range errCh {
		errs = append(errs, e.Error())
	}
	if len(errs) > 0 {
		logDebug("some repo fetches failed: %s", strings.Join(errs, "; "))
		return all, fmt.Errorf("partial: %s", strings.Join(errs, "; "))
	}

	logDebug("fetched prs count=%d (server)", len(all))
	return all, nil
}

func (p *bitbucketPlugin) mapServerPR(sp serverPR, repo bbRepo) bbPR {
	mapped := bbPR{
		ID:           sp.ID,
		Title:        sp.Title,
		State:        sp.State,
		CreatedOn:    millisToISO(sp.CreatedDate),
		UpdatedOn:    millisToISO(sp.UpdatedDate),
		SourceBranch: sp.FromRef.DisplayID,
		DestBranch:   sp.ToRef.DisplayID,
		SourceCommit: sp.FromRef.LatestCommit,
		Summary:      sp.Description,
		RepoSlug:     repo.Slug,
		Author: bbUser{
			UUID:        sp.Author.User.Name,
			Username:    sp.Author.User.Name,
			DisplayName: sp.Author.User.DisplayName,
		},
		DeepLink: fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d", p.webBase, p.workspace, repo.Slug, sp.ID),
	}
	for _, r := range sp.Reviewers {
		mapped.Reviewers = append(mapped.Reviewers, bbUser{
			UUID:        r.User.Name,
			Username:    r.User.Name,
			DisplayName: r.User.DisplayName,
		})
	}
	for _, pt := range sp.Participants {
		mapped.Participants = append(mapped.Participants, bbParticipant{
			User: bbUser{
				UUID:        pt.User.Name,
				Username:    pt.User.Name,
				DisplayName: pt.User.DisplayName,
			},
			Approved: pt.Approved,
			State:    pt.Status,
		})
	}
	return mapped
}

// --- fetchBuildStatuses ---

func (p *bitbucketPlugin) fetchBuildStatuses(prs []bbPR) map[int][]bbCommitStatus {
	if p.serverMode {
		return p.fetchBuildStatusesServer(prs)
	}
	return p.fetchBuildStatusesCloud(prs)
}

func (p *bitbucketPlugin) fetchBuildStatusesCloud(prs []bbPR) map[int][]bbCommitStatus {
	// Build statuses require repo slug which isn't available in the mapped PR.
	// Will be implemented when repo slug is added to the bbPR struct.
	return make(map[int][]bbCommitStatus)
}

func (p *bitbucketPlugin) fetchBuildStatusesServer(prs []bbPR) map[int][]bbCommitStatus {
	// Build statuses require repo slug which isn't available in the mapped PR.
	// Will be implemented when repo slug is added to the bbPR struct.
	return make(map[int][]bbCommitStatus)
}

// --- Filtering ---

func (p *bitbucketPlugin) filterPRs(all []bbPR) []bbPR {
	if p.currentUser == nil || p.currentUser.Username == "" {
		return nil
	}

	myID := p.currentUser.Username
	if p.currentUser.UUID != "" && !p.serverMode {
		myID = p.currentUser.UUID
	}

	var filtered []bbPR
	seen := make(map[int]bool)

	for _, pr := range all {
		if seen[pr.ID] {
			continue
		}
		seen[pr.ID] = true

		if p.isExcluded(pr.Author.Username) {
			continue
		}

		// Match by either UUID or username
		authorMatch := pr.Author.UUID == myID || pr.Author.Username == myID

		if p.config.ShowMyPRs && authorMatch {
			filtered = append(filtered, pr)
			continue
		}

		if p.config.ShowReviewRequests {
			for _, rev := range pr.Reviewers {
				if rev.UUID == myID || rev.Username == myID {
					filtered = append(filtered, pr)
					break
				}
			}
		}
	}

	return filtered
}

// --- Snapshot builders ---

func (p *bitbucketPlugin) buildItems(prs []bbPR, statuses map[int][]bbCommitStatus) []pluginItem {
	items := make([]pluginItem, 0, len(prs))
	for _, pr := range prs {
		repoName := pr.DestBranch
		if repoName == "" {
			repoName = pr.SourceBranch
		}
		severity := prSeverity(pr, p.currentUser, statuses)
		detail := prDetail(pr, p.currentUser, statuses)

		description := strings.TrimSpace(pr.Summary)
		if description != "" {
			detail = fmt.Sprintf("%s | %s", truncate(description, 160), detail)
		}

		deepLink := pr.DeepLink
		if deepLink == "" {
			deepLink = fmt.Sprintf("%s/%s/pull-requests/%d", p.webBase, repoName, pr.ID)
		}

		title := pr.Title
		if title == "" {
			title = pr.Summary
		}

		subtitle := fmt.Sprintf("%s · %s", repoName, pr.Author.DisplayName)

		items = append(items, pluginItem{
			ID:        fmt.Sprintf("%d", pr.ID),
			Title:     title,
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  severity,
			Timestamp: pr.UpdatedOn,
			DeepLink:  deepLink,
			Actions:   []pluginAction{{ID: "summarize", Label: "Summarize"}},
		})
	}
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

func (p *bitbucketPlugin) buildAlerts(prs []bbPR, statuses map[int][]bbCommitStatus) []pluginAlert {
	var alerts []pluginAlert

	failCount := 0
	for _, pr := range prs {
		if buildFailed(statuses[pr.ID]) {
			failCount++
		}
	}
	if failCount > 0 {
		alerts = append(alerts, pluginAlert{
			ID:       "bitbucket-build-fail",
			Severity: "critical",
			Message:  fmt.Sprintf("%d PR(s) with failing builds", failCount),
		})
	}

	changesRequested := 0
	for _, pr := range prs {
		if hasChangesRequested(pr, p.currentUser) {
			changesRequested++
		}
	}
	if changesRequested > 0 {
		alerts = append(alerts, pluginAlert{
			ID:       "bitbucket-changes-requested",
			Severity: "warning",
			Message:  fmt.Sprintf("%d PR(s) with changes requested", changesRequested),
		})
	}

	return alerts
}

func (p *bitbucketPlugin) buildSummary(items []pluginItem, total int) pluginSummary {
	if len(items) == 0 {
		return pluginSummary{
			Title:    "Bitbucket",
			Value:    "0 PRs",
			Trend:    "steady",
			Severity: "info",
			IconHint: "pull-request",
		}
	}

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

	value := fmt.Sprintf("%d", len(items))
	if total > p.config.MaxPRs {
		value = fmt.Sprintf("%d+", p.config.MaxPRs)
	}

	return pluginSummary{
		Title:    "Bitbucket",
		Value:    fmt.Sprintf("%s PRs", value),
		Trend:    "steady",
		Severity: severity,
		IconHint: "pull-request",
	}
}

// --- Event detection ---

func (p *bitbucketPlugin) detectEvents(prs []bbPR, statuses map[int][]bbCommitStatus) {
	currentStates := make(map[string]prStateInfo)
	for _, pr := range prs {
		key := fmt.Sprintf("%d", pr.ID)
		currentStates[key] = prStateInfo{hash: prStateHash(pr, statuses[pr.ID]), deepLink: pr.DeepLink}
	}

	prevKeys := p.prevPRStates

	for key, oldState := range prevKeys {
		newState, ok := currentStates[key]
		if !ok {
			logDebug("pr removed id=%s", key)
			emitEvent(pluginEvent{
				Type:      "pr.merged",
				PluginID:  "bitbucket",
				Message:   fmt.Sprintf("PR #%s was merged or closed", key),
				Severity:  "info",
				Data:      map[string]string{"prId": key, "url": oldState.deepLink},
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
			continue
		}
		if newState.hash != oldState.hash && newState.hash != "" && oldState.hash != "" {
			oldFailed := strings.Contains(oldState.hash, "build:failed")
			newFailed := strings.Contains(newState.hash, "build:failed")
			if !oldFailed && newFailed {
				emitEvent(pluginEvent{
					Type:      "pr.status_failed",
					PluginID:  "bitbucket",
					Message:   fmt.Sprintf("Build failed on PR #%s", key),
					Severity:  "critical",
					Data:      map[string]string{"prId": key, "url": newState.deepLink},
					Timestamp: time.Now().UTC().Format(time.RFC3339),
				})
			}
		}
	}

	for key, info := range currentStates {
		if _, ok := prevKeys[key]; !ok && len(prevKeys) > 0 {
			logDebug("new pr id=%s", key)
			emitEvent(pluginEvent{
				Type:      "pr.created",
				PluginID:  "bitbucket",
				Message:   fmt.Sprintf("New pull request #%s", key),
				Severity:  "info",
				Data:      map[string]string{"prId": key, "url": info.deepLink},
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			})
		}
	}

	p.prevPRStates = currentStates
}

// --- Helpers ---

func prSeverity(pr bbPR, currentUser *bbUser, statuses map[int][]bbCommitStatus) string {
	if buildFailed(statuses[pr.ID]) {
		return "critical"
	}
	if hasChangesRequested(pr, currentUser) {
		return "warning"
	}
	return "info"
}

func prDetail(pr bbPR, currentUser *bbUser, statuses map[int][]bbCommitStatus) string {
	parts := []string{
		fmt.Sprintf("%s → %s", pr.SourceBranch, pr.DestBranch),
	}

	approvals := countApprovals(pr)
	if approvals > 0 {
		parts = append(parts, fmt.Sprintf("%d ✓", approvals))
	}

	changesReq := countChangesRequested(pr)
	if changesReq > 0 {
		parts = append(parts, fmt.Sprintf("%d ✗", changesReq))
	}

	if currentUser != nil {
		myID := currentUser.Username
		if currentUser.UUID != "" {
			myID = currentUser.UUID
		}
		if pr.Author.UUID == myID || pr.Author.Username == myID {
			parts = append(parts, "author")
		} else {
			for _, rev := range pr.Reviewers {
				if rev.UUID == myID || rev.Username == myID {
					parts = append(parts, "reviewer")
					break
				}
			}
		}
	}

	sts := statuses[pr.ID]
	if len(sts) > 0 {
		latest := sts[0]
		for _, s := range sts[1:] {
			if s.CreatedOn > latest.CreatedOn {
				latest = s
			}
		}
		stateStr := strings.ToLower(latest.State)
		switch stateStr {
		case "successful":
			parts = append(parts, "CI:✓")
		case "failed", "stopped":
			parts = append(parts, "CI:✗")
		case "inprogress":
			parts = append(parts, "CI:⋯")
		}
	}

	return strings.Join(parts, " | ")
}

func countApprovals(pr bbPR) int {
	count := 0
	for _, p := range pr.Participants {
		if p.Approved {
			count++
		}
	}
	return count
}

func countChangesRequested(pr bbPR) int {
	count := 0
	for _, p := range pr.Participants {
		if p.State == "changes_requested" {
			count++
		}
	}
	return count
}

func hasChangesRequested(pr bbPR, currentUser *bbUser) bool {
	if currentUser == nil {
		return false
	}
	for _, p := range pr.Participants {
		if p.State == "changes_requested" {
			return true
		}
	}
	return false
}

func buildFailed(statuses []bbCommitStatus) bool {
	for _, s := range statuses {
		if s.State == "FAILED" || s.State == "STOPPED" {
			return true
		}
	}
	return false
}

func prStateHash(pr bbPR, statuses []bbCommitStatus) string {
	buildState := "build:unknown"
	if len(statuses) > 0 {
		latest := statuses[0]
		for _, s := range statuses[1:] {
			if s.CreatedOn > latest.CreatedOn {
				latest = s
			}
		}
		buildState = "build:" + strings.ToLower(latest.State)
	}

	reviewState := "review:open"
	for _, p := range pr.Participants {
		if p.Approved {
			reviewState = "review:approved"
			break
		}
		if p.State == "changes_requested" {
			reviewState = "review:changes_requested"
			break
		}
	}

	return fmt.Sprintf("state:%s|%s|%s", pr.State, reviewState, buildState)
}

func (p *bitbucketPlugin) isExcluded(username string) bool {
	for _, ex := range p.config.ExcludedUsers {
		if strings.EqualFold(strings.TrimSpace(ex), strings.TrimSpace(username)) {
			return true
		}
	}
	return false
}

func millisToISO(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.Unix(ms/1000, (ms%1000)*1000000).UTC().Format(time.RFC3339)
}

func parseConfig(cfg map[string]string) bitbucketConfig {
	config := bitbucketConfig{
		ShowMyPRs:         true,
		ShowReviewRequests: true,
		MaxPRs:            10,
	}
	if v, ok := cfg["serverUrl"]; ok {
		config.ServerURL = normalizeServerURL(v)
	}
	if v, ok := cfg["username"]; ok {
		config.Username = strings.TrimSpace(v)
	}
	if v, ok := cfg["workspace"]; ok {
		config.Workspace = strings.TrimSpace(v)
	}
	if v, ok := cfg["showMyPRs"]; ok {
		config.ShowMyPRs = parseBool(v, true)
	}
	if v, ok := cfg["showReviewRequests"]; ok {
		config.ShowReviewRequests = parseBool(v, false)
	}
	if v, ok := cfg["showPipelines"]; ok {
		config.ShowPipelines = parseBool(v, false)
	}
	if v, ok := cfg["maxPRs"]; ok {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			config.MaxPRs = n
		}
	}
	if v, ok := cfg["excludedUsers"]; ok {
		for _, u := range strings.Split(v, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				config.ExcludedUsers = append(config.ExcludedUsers, u)
			}
		}
	}
	return config
}

func normalizeServerURL(raw string) string {
	d := strings.TrimSpace(raw)
	if d == "" {
		return ""
	}
	if !strings.HasPrefix(d, "https://") && !strings.HasPrefix(d, "http://") {
		d = "https://" + d
	}
	return strings.TrimRight(d, "/")
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
