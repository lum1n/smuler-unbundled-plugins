package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "bitbucket"
	pluginVersion = "0.1.3"
	cloudAPIBase  = "https://api.bitbucket.org/2.0"
	cloudWebBase  = "https://bitbucket.org"
	// Browser session cookies are scoped to bitbucket.org, not
	// api.bitbucket.org; the web app's same-origin API proxy accepts them.
	cloudCookieAPIBase = "https://bitbucket.org/!api/2.0"

	// maxConcurrentRequests bounds per-repo / per-PR fan-out so large
	// workspaces don't trip Bitbucket's rate limiter.
	maxConcurrentRequests = 6
	// repoCacheTTL controls how often the repo list and current user are
	// re-fetched; they rarely change and cost several paginated requests.
	repoCacheTTL = 10 * time.Minute
	// maxErrorBodyLen caps how much of an error response body is kept for
	// alerts and debug logs.
	maxErrorBodyLen = 200
)

type apiHTTPError struct {
	StatusCode int
	RetryAfter int
	Body       string
}

func (e *apiHTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// errorSnippet returns a short, single-line excerpt of a response body for
// error messages; full bodies are never surfaced or logged.
func errorSnippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	return truncate(s, maxErrorBodyLen)
}

// isLoginPage reports whether a response is an HTML page (typically a
// redirect to the Atlassian login screen after a session cookie expired)
// rather than an API payload.
func isLoginPage(resp *http.Response) bool {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/html") {
		return true
	}
	if resp.Request != nil && resp.Request.URL != nil {
		u := resp.Request.URL
		host := strings.ToLower(u.Host)
		path := strings.ToLower(u.Path)
		if host == "id.atlassian.com" || strings.Contains(path, "/login") || strings.Contains(path, "/account/signin") {
			return true
		}
	}
	return false
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
	Action  string     `json:"action"`
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
	Values        []json.RawMessage `json:"values"`
	Size          int               `json:"size"`
	IsLastPage    bool              `json:"isLastPage"`
	NextPageStart *int              `json:"nextPageStart"`
}

type serverRepo struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
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
		DisplayID    string `json:"displayId"`
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
	userFetched  time.Time
	repos        []bbRepo
	reposFetched time.Time
	prevPRStates map[string]prStateInfo
	prCache      map[string]bbPR // prKey -> metadata for getMRDiff lookups
}

func (p *bitbucketPlugin) resolveBases() {
	if p.config.ServerURL != "" {
		p.apiBase = p.config.ServerURL + "/rest/api/1.0"
		p.webBase = p.config.ServerURL
		p.serverMode = true
	} else {
		p.apiBase = cloudAPIBase
		if p.auth.Kind == "browser_import" {
			p.apiBase = cloudCookieAPIBase
		}
		p.webBase = cloudWebBase
		p.serverMode = false
	}
}

// invalidateCaches drops cached user/repo data so the next refresh
// re-validates credentials from scratch.
func (p *bitbucketPlugin) invalidateCaches() {
	p.currentUser = nil
	p.userFetched = time.Time{}
	p.repos = nil
	p.reposFetched = time.Time{}
}

// prKey uniquely identifies a PR within a workspace. PR numbers are only
// unique per repository, so the repo slug must be part of the key.
func prKey(pr bbPR) string {
	return fmt.Sprintf("%s#%d", pr.RepoSlug, pr.ID)
}

// lookupCachedPR resolves an item id ("repo#123", or a bare "123" when the
// number is unambiguous) to a cached PR.
func (p *bitbucketPlugin) lookupCachedPR(id string) (bbPR, bool) {
	if pr, ok := p.prCache[id]; ok {
		return pr, true
	}
	n, err := strconv.Atoi(id)
	if err != nil {
		return bbPR{}, false
	}
	var found bbPR
	matches := 0
	for _, pr := range p.prCache {
		if pr.ID == n {
			found = pr
			matches++
		}
	}
	return found, matches == 1
}

func (p *bitbucketPlugin) Initialize(params sdk.InitializeParams) string {
	p.config = parseConfig(params.Config)
	p.workspace = strings.TrimSpace(p.config.Workspace)
	p.auth = bitbucketAuth{}
	p.invalidateCaches()

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

	p.resolveBases()

	// In server mode, derive username from config or from app password
	if p.serverMode && p.config.Username == "" {
		if u := p.auth.username(); u != "" {
			p.config.Username = u
		}
	}

	sdk.Log("initialize workspace=%q authKind=%s serverMode=%t username=%q showMyPRs=%t showReviewRequests=%t showPipelines=%t max=%d",
		p.workspace, p.auth.Kind, p.serverMode, p.config.Username, p.config.ShowMyPRs, p.config.ShowReviewRequests,
		p.config.ShowPipelines, p.config.MaxPRs)

	return sdk.HealthOK
}

func (p *bitbucketPlugin) GetStatus() sdk.Snapshot {
	return p.buildSnapshot()
}

func (p *bitbucketPlugin) PerformAction(id string, params map[string]string) (bool, string) {
	return false, "use PerformActionResult"
}

func (p *bitbucketPlugin) PerformActionResult(id string, params map[string]string) sdk.ActionResult {
	if p.workspace == "" {
		return sdk.ActionFail("no Bitbucket workspace configured")
	}
	if !p.auth.hasAuth() {
		return sdk.ActionFail("no Bitbucket auth configured")
	}

	switch id {
	case "getPRDetails":
		idStr := strings.TrimSpace(params["id"])
		repoSlug := strings.TrimSpace(params["repoSlug"])
		if idStr == "" || repoSlug == "" {
			return sdk.ActionFail("missing id or repoSlug payload")
		}
		prID, err := strconv.Atoi(idStr)
		if err != nil {
			return sdk.ActionFail("invalid id payload")
		}
		return p.getPRDetails(prID, repoSlug)
	case "getMRDiff":
		idStr := strings.TrimSpace(params["id"])
		if idStr == "" {
			return sdk.ActionFail("missing id payload")
		}
		return p.getMRDiff(idStr)
	case "summarize":
		idStr := strings.TrimSpace(params["id"])
		if idStr == "" {
			return sdk.ActionFail("missing id payload")
		}
		return p.summarizePR(idStr)
	case "searchPRs":
		query := strings.TrimSpace(params["query"])
		if query == "" {
			return sdk.ActionFail("missing query payload")
		}
		return p.searchPRs(query)
	default:
		return sdk.ActionFail(fmt.Sprintf("unknown action: %s", id))
	}
}

func (p *bitbucketPlugin) Shutdown() {}

func (p *bitbucketPlugin) getPRDetails(id int, repoSlug string) sdk.ActionResult {
	if p.serverMode {
		return p.getPRDetailsServer(id, repoSlug)
	}
	return p.getPRDetailsCloud(id, repoSlug)
}

func (p *bitbucketPlugin) getPRDetailsCloud(id int, repoSlug string) sdk.ActionResult {
	base := fmt.Sprintf("%s/repositories/%s/%s/pullrequests/%d", p.apiBase, p.workspace, repoSlug, id)

	var pr cloudPR
	if err := p.getJSON(base, &pr); err != nil {
		return sdk.ActionFail(err.Error())
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

	return p.formatPRDetails(mapped, comments, diffStat)
}

func (p *bitbucketPlugin) getPRDetailsServer(id int, repoSlug string) sdk.ActionResult {
	base := fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d", p.apiBase, p.workspace, repoSlug, id)

	var sp serverPR
	if err := p.getJSON(base, &sp); err != nil {
		return sdk.ActionFail(err.Error())
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

	return p.formatPRDetails(mapped, comments, diffStat)
}

func (p *bitbucketPlugin) formatPRDetails(pr bbPR, comments, diffStat []string) sdk.ActionResult {
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

	return sdk.ActionOK(strings.Join(lines, "\n"))
}

func (p *bitbucketPlugin) getMRDiff(id string) sdk.ActionResult {
	pr, ok := p.lookupCachedPR(id)
	if !ok {
		return sdk.ActionFail(fmt.Sprintf("PR %s not found in cache", id))
	}

	var url string
	if p.serverMode {
		url = fmt.Sprintf("%s/projects/%s/repos/%s/pull-requests/%d/diff", p.apiBase, p.workspace, pr.RepoSlug, pr.ID)
	} else {
		url = fmt.Sprintf("%s/repositories/%s/%s/pullrequests/%d/diff", p.apiBase, p.workspace, pr.RepoSlug, pr.ID)
	}

	diff, err := p.fetchRaw(url)
	if err != nil {
		return sdk.ActionFail(fmt.Sprintf("failed to fetch diff: %v", err))
	}
	return sdk.ActionOK(diff)
}

func (p *bitbucketPlugin) summarizePR(id string) sdk.ActionResult {
	pr, ok := p.lookupCachedPR(id)
	if !ok {
		return sdk.ActionFail(fmt.Sprintf("PR %s not found in cache", id))
	}

	diffResult := p.getMRDiff(id)
	if !diffResult.Success {
		return diffResult
	}
	if strings.TrimSpace(diffResult.Data) == "" {
		return sdk.ActionFail("Empty diff")
	}

	title := pr.Title
	if title == "" {
		title = pr.Summary
	}
	if title == "" {
		title = fmt.Sprintf("PR #%d", pr.ID)
	}
	return sdk.ActionAITask(sdk.AIWindowOpts{
		ID:       fmt.Sprintf("bitbucket.summarize.%s.%d", pr.RepoSlug, pr.ID),
		Title:    title,
		Subtitle: "Bitbucket",
		IconHint: "arrow.triangle.pull",
		Task:     sdk.AITaskSummarizeBullets,
		Input:    diffResult.Data,
	})
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
		return "", &apiHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       errorSnippet(body),
		}
	}
	if isLoginPage(resp) {
		return "", &apiHTTPError{StatusCode: http.StatusUnauthorized, Body: "session expired (redirected to login page)"}
	}
	return string(body), nil
}

func (p *bitbucketPlugin) searchPRs(query string) sdk.ActionResult {
	repos, err := p.cachedRepos()
	if err != nil {
		return sdk.ActionFail(err.Error())
	}

	var all []bbPR
	if p.serverMode {
		all, err = p.fetchAllPRsServer(repos)
	} else {
		all, err = p.fetchAllPRsCloud(repos)
	}
	if err != nil && !strings.HasPrefix(err.Error(), "partial:") {
		return sdk.ActionFail(err.Error())
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
		return sdk.ActionOK("No matching pull requests found.")
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

	return sdk.ActionOK(strings.Join(lines, "\n"))
}

func (p *bitbucketPlugin) buildSnapshot() sdk.Snapshot {
	if p.workspace == "" {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateDegraded,
			Summary:      sdk.Summary{Title: "Bitbucket", Value: "No workspace", Trend: sdk.TrendSteady, Severity: sdk.SeverityInfo, IconHint: "pull-request"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	if !p.auth.hasAuth() {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateError,
			Summary:      sdk.Summary{Title: "Bitbucket", Value: "No auth", Trend: sdk.TrendSteady, Severity: sdk.SeverityWarning, IconHint: "pull-request"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "bitbucket-no-auth", Severity: sdk.SeverityWarning, Message: "Connect a Bitbucket account via Settings to see pull requests."}},
			RefreshAfter: 120,
			Health:       sdk.HealthAuthReq,
		}
	}

	user := p.currentUser
	if user == nil || time.Since(p.userFetched) > repoCacheTTL {
		fetched, err := p.fetchCurrentUser()
		if err != nil {
			sdk.Log("fetchCurrentUser error: %v", err)
			return p.snapshotFromAPIError("Auth err", err)
		}
		user = fetched
		p.currentUser = user
		p.userFetched = time.Now()
	}

	if user.Username == "" {
		return sdk.Snapshot{
			PluginID:     pluginID,
			State:        sdk.StateDegraded,
			Summary:      sdk.Summary{Title: "Bitbucket", Value: "No username", Trend: sdk.TrendSteady, Severity: sdk.SeverityWarning, IconHint: "pull-request"},
			Items:        []sdk.Item{},
			Actions:      []sdk.Action{},
			Alerts:       []sdk.Alert{{ID: "bitbucket-no-username", Severity: sdk.SeverityWarning, Message: "Set your Bitbucket username in plugin settings to filter pull requests."}},
			RefreshAfter: 120,
			Health:       sdk.HealthDegraded,
		}
	}

	repos, err := p.cachedRepos()
	if err != nil {
		sdk.Log("fetchRepos error: %v", err)
		return p.snapshotFromAPIError("API err", err)
	}

	allPRs, err := p.fetchAllPRs(repos)
	var partialFetchErrors string
	if err != nil {
		sdk.Log("fetchAllPRs error: %v", err)
		if strings.HasPrefix(err.Error(), "partial:") {
			partialFetchErrors = strings.TrimPrefix(err.Error(), "partial: ")
		} else {
			return p.snapshotFromAPIError("API err", err)
		}
	}

	filtered := p.filterPRs(allPRs)
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].UpdatedOn > filtered[j].UpdatedOn
	})
	totalMatching := len(filtered)
	if len(filtered) > p.config.MaxPRs {
		filtered = filtered[:p.config.MaxPRs]
	}

	prStatuses := make(map[string][]bbCommitStatus)
	if p.config.ShowPipelines {
		prStatuses = p.fetchBuildStatuses(filtered)
	}

	items := p.buildItems(filtered, prStatuses)
	alerts := p.buildAlerts(filtered, prStatuses)
	summary := p.buildSummary(items, totalMatching)

	// Rebuild PR cache for getMRDiff lookups
	p.prCache = make(map[string]bbPR)
	for _, pr := range filtered {
		p.prCache[prKey(pr)] = pr
	}

	if partialFetchErrors != "" {
		alerts = append([]sdk.Alert{{ID: "bitbucket-partial-fetch", Severity: sdk.SeverityWarning, Message: "Some repositories could not be fetched: " + partialFetchErrors}}, alerts...)
	}

	if items == nil {
		items = []sdk.Item{}
	}
	if alerts == nil {
		alerts = []sdk.Alert{}
	}

	p.detectEvents(filtered, prStatuses)

	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateReady,
		Summary:      summary,
		Items:        items,
		Actions:      []sdk.Action{},
		Alerts:       alerts,
		RefreshAfter: 120,
		Health:       sdk.HealthOK,
	}
}

func (p *bitbucketPlugin) degradedSnapshot(value, message, health string, retryAfter int) sdk.Snapshot {
	if health == "" {
		health = sdk.HealthDegraded
	}
	return sdk.Snapshot{
		PluginID:     pluginID,
		State:        sdk.StateDegraded,
		Summary:      sdk.Summary{Title: "Bitbucket", Value: value, Trend: sdk.TrendSteady, Severity: sdk.SeverityWarning, IconHint: "pull-request"},
		Items:        []sdk.Item{},
		Actions:      []sdk.Action{},
		Alerts:       []sdk.Alert{{ID: "bitbucket-api-err", Severity: sdk.SeverityWarning, Message: message}},
		RefreshAfter: httphealth.DefaultRefreshAfter(health, retryAfter),
		Health:       health,
	}
}

func (p *bitbucketPlugin) snapshotFromAPIError(value string, err error) sdk.Snapshot {
	var apiErr *apiHTTPError
	if errors.As(err, &apiErr) {
		health := httphealth.ClassifyHTTPStatus(apiErr.StatusCode)
		message := "Could not reach Bitbucket: " + apiErr.Error()
		if health == sdk.HealthAuthReq {
			p.invalidateCaches()
			message = "Bitbucket authentication failed — reconnect in Settings"
		} else if health == sdk.HealthRateLimited {
			message = "Bitbucket API rate limited"
		}
		return p.degradedSnapshot(value, message, health, apiErr.RetryAfter)
	}
	return p.degradedSnapshot(value, "Could not reach Bitbucket: "+err.Error(), sdk.HealthDegraded, 0)
}

// --- HTTP helpers ---

func (p *bitbucketPlugin) doAPI(method, urlStr string) (*http.Response, error) {
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "smuler-bitbucket-plugin/"+pluginVersion)
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
			Body:       errorSnippet(body),
		}
	}
	if isLoginPage(resp) {
		return &apiHTTPError{StatusCode: http.StatusUnauthorized, Body: "session expired (redirected to login page)"}
	}

	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
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
	sdk.Log("current user (cloud) uuid=%s username=%s", cu.UUID, cu.Username)
	return &bbUser{UUID: cu.UUID, Username: cu.Username, DisplayName: cu.DisplayName}, nil
}

func (p *bitbucketPlugin) fetchCurrentUserServer() (*bbUser, error) {
	username := p.config.Username

	// If we have a username from config/app-password, try to look up the user
	if username != "" {
		url := fmt.Sprintf("%s/users/%s", p.apiBase, neturl.PathEscape(username))
		var su serverUser
		if err := p.getJSON(url, &su); err != nil {
			sdk.Log("server user lookup failed for %q: %v", username, err)
			var apiErr *apiHTTPError
			if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
				return nil, err
			}
		} else {
			sdk.Log("current user (server) name=%s displayName=%s", su.Name, su.DisplayName)
			return &bbUser{UUID: su.Name, Username: su.Name, DisplayName: su.DisplayName}, nil
		}
	}

	// Try X-Ausername header from a lightweight request
	resp, headers, err := p.doAPIGetHeaders(p.apiBase + "/application-properties")
	if err != nil {
		return nil, fmt.Errorf("api request: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, &apiHTTPError{StatusCode: resp.StatusCode, RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if isLoginPage(resp) {
			return nil, &apiHTTPError{StatusCode: http.StatusUnauthorized, Body: "session expired (redirected to login page)"}
		}
		if h := headers.Get("X-Ausername"); h != "" {
			sdk.Log("current user (server, from header) username=%s", h)
			return &bbUser{UUID: h, Username: h, DisplayName: h}, nil
		}
	}

	return nil, fmt.Errorf("could not determine Bitbucket username — set it in plugin settings")
}

// --- fetchRepos ---

// cachedRepos returns the repo list, re-fetching it at most every
// repoCacheTTL; the paginated listing is the most expensive part of a refresh.
func (p *bitbucketPlugin) cachedRepos() ([]bbRepo, error) {
	if p.repos != nil && time.Since(p.reposFetched) < repoCacheTTL {
		return p.repos, nil
	}
	repos, err := p.fetchRepos()
	if err != nil {
		return nil, err
	}
	if repos == nil {
		repos = []bbRepo{}
	}
	p.repos = repos
	p.reposFetched = time.Now()
	return repos, nil
}

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

	sdk.Log("fetched repos count=%d (cloud)", len(all))
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
		if page.IsLastPage || len(page.Values) == 0 {
			break
		}
		if page.NextPageStart != nil {
			start = *page.NextPageStart
		} else {
			start += len(page.Values)
		}
	}

	sdk.Log("fetched repos count=%d (server)", len(all))
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

	sem := make(chan struct{}, maxConcurrentRequests)
	for _, repo := range repos {
		wg.Add(1)
		go func(r bbRepo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nextURL := fmt.Sprintf("%s/repositories/%s/%s/pullrequests?state=OPEN&pagelen=50&fields=%s",
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

	if err := combineFetchErrors(errCh, len(repos)); err != nil {
		return all, err
	}

	sdk.Log("fetched prs count=%d (cloud)", len(all))
	return all, nil
}

// combineFetchErrors turns per-repo failures into a single error. When every
// repo failed (e.g. revoked credentials or rate limiting) the underlying
// error is returned so the snapshot reports auth_required / rate_limited;
// otherwise a "partial:" error lets the caller show the PRs it did get.
func combineFetchErrors(errCh <-chan error, total int) error {
	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	if len(errs) == 0 {
		return nil
	}
	for _, e := range errs {
		var apiErr *apiHTTPError
		if errors.As(e, &apiErr) {
			switch httphealth.ClassifyHTTPStatus(apiErr.StatusCode) {
			case httphealth.HealthAuthReq, httphealth.HealthRateLimited:
				return e
			}
		}
	}
	if len(errs) == total {
		return errs[0]
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	sdk.Log("some repo fetches failed: %s", strings.Join(msgs, "; "))
	return fmt.Errorf("partial: %s", strings.Join(msgs, "; "))
}

func (p *bitbucketPlugin) mapCloudPR(pr cloudPR, repoSlug string) bbPR {
	mapped := bbPR{
		ID:           pr.ID,
		Title:        pr.Title,
		State:        pr.State,
		CreatedOn:    normalizeTimestamp(pr.CreatedOn),
		UpdatedOn:    normalizeTimestamp(pr.UpdatedOn),
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
	for _, pt := range pr.Participants {
		mapped.Participants = append(mapped.Participants, bbParticipant{
			User:     bbUser{UUID: pt.User.UUID, Username: pt.User.Username, DisplayName: pt.User.DisplayName},
			Approved: pt.Approved,
			State:    pt.State,
		})
	}
	return mapped
}

func (p *bitbucketPlugin) fetchAllPRsServer(repos []bbRepo) ([]bbPR, error) {
	var all []bbPR
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, len(repos))

	sem := make(chan struct{}, maxConcurrentRequests)
	for _, repo := range repos {
		wg.Add(1)
		go func(r bbRepo) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
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
						sdk.Log("unmarshal server pr: %v", err)
						continue
					}
					all = append(all, p.mapServerPR(sp, r))
				}
				mu.Unlock()

				if page.IsLastPage || len(page.Values) == 0 {
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

	if err := combineFetchErrors(errCh, len(repos)); err != nil {
		return all, err
	}

	sdk.Log("fetched prs count=%d (server)", len(all))
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
			State:    normalizeServerReviewStatus(pt.Status),
		})
	}
	return mapped
}

// --- fetchBuildStatuses ---

// fetchBuildStatuses loads commit/build statuses for the (already capped)
// visible PRs with bounded parallelism. Failures are logged and skipped so a
// missing pipeline scope never blanks the PR list.
func (p *bitbucketPlugin) fetchBuildStatuses(prs []bbPR) map[string][]bbCommitStatus {
	result := make(map[string][]bbCommitStatus)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentRequests)
	for _, pr := range prs {
		wg.Add(1)
		go func(pr bbPR) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var sts []bbCommitStatus
			var err error
			if p.serverMode {
				sts, err = p.fetchBuildStatusesServer(pr)
			} else {
				sts, err = p.fetchBuildStatusesCloud(pr)
			}
			if err != nil {
				sdk.Log("build statuses %s: %v", prKey(pr), err)
				return
			}
			if len(sts) > 0 {
				mu.Lock()
				result[prKey(pr)] = sts
				mu.Unlock()
			}
		}(pr)
	}
	wg.Wait()
	return result
}

func (p *bitbucketPlugin) fetchBuildStatusesCloud(pr bbPR) ([]bbCommitStatus, error) {
	url := fmt.Sprintf("%s/repositories/%s/%s/pullrequests/%d/statuses?pagelen=25", p.apiBase, p.workspace, pr.RepoSlug, pr.ID)
	var page cloudPaginatedStatuses
	if err := p.getJSON(url, &page); err != nil {
		return nil, err
	}
	out := make([]bbCommitStatus, 0, len(page.Values))
	for _, s := range page.Values {
		out = append(out, bbCommitStatus{
			Key:       s.Key,
			Name:      s.Name,
			State:     strings.ToUpper(s.State),
			URL:       s.URL,
			CreatedOn: normalizeTimestamp(s.CreatedOn),
		})
	}
	return out, nil
}

func (p *bitbucketPlugin) fetchBuildStatusesServer(pr bbPR) ([]bbCommitStatus, error) {
	if pr.SourceCommit == "" {
		return nil, nil
	}
	url := fmt.Sprintf("%s/rest/build-status/1.0/commits/%s?limit=25", p.webBase, neturl.PathEscape(pr.SourceCommit))
	var page serverPaginatedResponse
	if err := p.getJSON(url, &page); err != nil {
		return nil, err
	}
	out := make([]bbCommitStatus, 0, len(page.Values))
	for _, raw := range page.Values {
		var b serverBuild
		if err := json.Unmarshal(raw, &b); err != nil {
			continue
		}
		out = append(out, bbCommitStatus{
			Key:       b.Key,
			Name:      b.Name,
			State:     strings.ToUpper(b.State),
			URL:       b.URL,
			CreatedOn: millisToISO(b.BuildDate),
		})
	}
	return out, nil
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
	seen := make(map[string]bool)

	for _, pr := range all {
		key := prKey(pr)
		if seen[key] {
			continue
		}
		seen[key] = true

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

func (p *bitbucketPlugin) buildItems(prs []bbPR, statuses map[string][]bbCommitStatus) []sdk.Item {
	items := make([]sdk.Item, 0, len(prs))
	for _, pr := range prs {
		repoName := pr.RepoSlug
		severity := prSeverity(pr, p.currentUser, statuses)
		detail := prDetail(pr, p.currentUser, statuses)

		description := strings.TrimSpace(pr.Summary)
		if description != "" {
			detail = fmt.Sprintf("%s | %s", truncate(description, 160), detail)
		}

		deepLink := pr.DeepLink
		if deepLink == "" {
			deepLink = fmt.Sprintf("%s/%s/%s/pull-requests/%d", p.webBase, p.workspace, repoName, pr.ID)
		}

		title := pr.Title
		if title == "" {
			title = pr.Summary
		}

		subtitle := fmt.Sprintf("%s · %s", repoName, pr.Author.DisplayName)

		items = append(items, sdk.Item{
			ID:        prKey(pr),
			Title:     title,
			Subtitle:  subtitle,
			Detail:    detail,
			Severity:  severity,
			Timestamp: pr.UpdatedOn,
			DeepLink:  deepLink,
			Actions:   []sdk.Action{{ID: "summarize", Label: "Summarize"}},
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

func (p *bitbucketPlugin) buildAlerts(prs []bbPR, statuses map[string][]bbCommitStatus) []sdk.Alert {
	var alerts []sdk.Alert

	failCount := 0
	for _, pr := range prs {
		if buildFailed(statuses[prKey(pr)]) {
			failCount++
		}
	}
	if failCount > 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "bitbucket-build-fail",
			Severity: sdk.SeverityCritical,
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
		alerts = append(alerts, sdk.Alert{
			ID:       "bitbucket-changes-requested",
			Severity: sdk.SeverityWarning,
			Message:  fmt.Sprintf("%d PR(s) with changes requested", changesRequested),
		})
	}

	return alerts
}

func (p *bitbucketPlugin) buildSummary(items []sdk.Item, total int) sdk.Summary {
	if len(items) == 0 {
		return sdk.Summary{
			Title:    "Bitbucket",
			Value:    "0 PRs",
			Trend:    sdk.TrendSteady,
			Severity: sdk.SeverityInfo,
			IconHint: "pull-request",
		}
	}

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

	value := fmt.Sprintf("%d", len(items))
	if total > p.config.MaxPRs {
		value = fmt.Sprintf("%d+", p.config.MaxPRs)
	}

	return sdk.Summary{
		Title:    "Bitbucket",
		Value:    fmt.Sprintf("%s PRs", value),
		Trend:    sdk.TrendSteady,
		Severity: severity,
		IconHint: "pull-request",
	}
}

// --- Event detection ---

func (p *bitbucketPlugin) detectEvents(prs []bbPR, statuses map[string][]bbCommitStatus) {
	currentStates := make(map[string]prStateInfo)
	for _, pr := range prs {
		key := prKey(pr)
		currentStates[key] = prStateInfo{hash: prStateHash(pr, statuses[key]), deepLink: pr.DeepLink}
	}

	prevKeys := p.prevPRStates

	for key, oldState := range prevKeys {
		newState, ok := currentStates[key]
		if !ok {
			sdk.Log("pr removed id=%s", key)
			sdk.Emit(sdk.Event{
				Type:     "pr.merged",
				Message:  fmt.Sprintf("PR %s was merged or closed", key),
				Severity: sdk.SeverityInfo,
				Data:     map[string]string{"prId": key, "url": oldState.deepLink},
			})
			continue
		}
		if newState.hash != oldState.hash && newState.hash != "" && oldState.hash != "" {
			oldFailed := strings.Contains(oldState.hash, "build:failed")
			newFailed := strings.Contains(newState.hash, "build:failed")
			if !oldFailed && newFailed {
				sdk.Emit(sdk.Event{
					Type:     "pr.status_failed",
					Message:  fmt.Sprintf("Build failed on PR %s", key),
					Severity: sdk.SeverityCritical,
					Data:     map[string]string{"prId": key, "url": newState.deepLink},
				})
			}
		}
	}

	for key, info := range currentStates {
		if _, ok := prevKeys[key]; !ok && len(prevKeys) > 0 {
			sdk.Log("new pr id=%s", key)
			sdk.Emit(sdk.Event{
				Type:     "pr.created",
				Message:  fmt.Sprintf("New pull request %s", key),
				Severity: sdk.SeverityInfo,
				Data:     map[string]string{"prId": key, "url": info.deepLink},
			})
		}
	}

	p.prevPRStates = currentStates
}

// --- Helpers ---

func prSeverity(pr bbPR, currentUser *bbUser, statuses map[string][]bbCommitStatus) string {
	if buildFailed(statuses[prKey(pr)]) {
		return sdk.SeverityCritical
	}
	if hasChangesRequested(pr, currentUser) {
		return sdk.SeverityWarning
	}
	return sdk.SeverityInfo
}

func prDetail(pr bbPR, currentUser *bbUser, statuses map[string][]bbCommitStatus) string {
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

	sts := statuses[prKey(pr)]
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

// normalizeTimestamp converts Bitbucket Cloud timestamps (RFC 3339 with
// microseconds and "+00:00") to plain UTC RFC 3339 for the host.
func normalizeTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format(time.RFC3339)
}

// normalizeServerReviewStatus maps Bitbucket Server participant statuses to
// the Cloud vocabulary used by the review helpers.
func normalizeServerReviewStatus(status string) string {
	switch strings.ToUpper(status) {
	case "NEEDS_WORK":
		return "changes_requested"
	case "APPROVED":
		return "approved"
	default:
		return strings.ToLower(status)
	}
}

func millisToISO(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.Unix(ms/1000, (ms%1000)*1000000).UTC().Format(time.RFC3339)
}

func parseConfig(cfg map[string]string) bitbucketConfig {
	config := bitbucketConfig{
		ShowMyPRs:          true,
		ShowReviewRequests: true,
		MaxPRs:             10,
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

func main() {
	sdk.Run(pluginID, pluginVersion, &bitbucketPlugin{
		client: &http.Client{Timeout: 25 * time.Second},
		config: bitbucketConfig{
			ShowMyPRs:          true,
			ShowReviewRequests: true,
			MaxPRs:             10,
		},
		prevPRStates: make(map[string]prStateInfo),
		prCache:      make(map[string]bbPR),
	})
}
