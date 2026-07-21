package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/httphealth"
	"github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "confluence"
	pluginVersion = "0.1.0"
	defaultMax    = 10
)

type handler struct {
	client *http.Client

	mu sync.Mutex

	domain   string
	space    string
	customCQL string
	maxItems int
	auth     confluenceAuth
	cloudID  string
	peerKeys map[string]struct{}

	prevPages map[string]prevPageInfo
	prevReady bool
}

type prevPageInfo struct {
	title   string
	url     string
	version int
	space   string
	keys    string
}

type confluencePage struct {
	ID               string
	Title            string
	Type             string
	SpaceKey         string
	SpaceName        string
	Version          int
	LastModified     string
	LastUpdater      string
	WebURL           string
	Excerpt          string
	Labels           []string
	RelatedIssueKeys []string
}

func (h *handler) Initialize(params sdk.InitializeParams) string {
	h.domain = normalizeDomain(params.Config["domain"])
	h.space = strings.TrimSpace(params.Config["space"])
	h.customCQL = strings.TrimSpace(params.Config["cql"])
	h.maxItems = parseMaxItems(params.Config["maxItems"], defaultMax)
	h.auth = confluenceAuth{}
	h.cloudID = ""

	for _, pa := range params.ProviderAuths {
		switch pa.Kind {
		case "api_key":
			if pa.APIKey != "" {
				h.auth.Kind = "api_key"
				h.auth.APIKey = pa.APIKey
			}
		case "oauth":
			if pa.AccessToken != "" {
				h.auth.Kind = "oauth"
				h.auth.AccessToken = pa.AccessToken
			}
		case "browser_import":
			if pa.CookieHeader != "" {
				h.auth.Kind = "browser_import"
				h.auth.CookieHeader = pa.CookieHeader
			}
		}
	}

	h.peerKeys = collectPeerIssueKeys(toPeerLite(params.PeerSnapshots))

	sdk.Log("initialize domain=%q space=%q max=%d auth=%q peers=%d",
		h.domain, h.space, h.maxItems, h.auth.Kind, len(h.peerKeys))

	if h.domain == "" {
		return sdk.HealthAuthReq
	}
	if !h.auth.hasAuth() {
		return sdk.HealthAuthReq
	}
	return sdk.HealthOK
}

func toPeerLite(snaps []sdk.Snapshot) []peerSnapshotLite {
	out := make([]peerSnapshotLite, 0, len(snaps))
	for _, s := range snaps {
		items := make([]peerItemLite, 0, len(s.Items))
		for _, it := range s.Items {
			items = append(items, peerItemLite{
				ID:       it.ID,
				Title:    it.Title,
				Subtitle: it.Subtitle,
				Detail:   it.Detail,
				Metadata: it.Metadata,
			})
		}
		out = append(out, peerSnapshotLite{PluginID: s.PluginID, Items: items})
	}
	return out
}

func (h *handler) GetStatus() sdk.Snapshot {
	if h.domain == "" {
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title:    "Confluence",
				Value:    "No domain",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityInfo,
				IconHint: "doc",
			},
			Alerts: []sdk.Alert{{
				ID:       "confluence-config",
				Severity: sdk.SeverityInfo,
				Message:  "Set Confluence domain in Settings",
			}},
			RefreshAfter: 300,
			Health:       sdk.HealthAuthReq,
		}
	}
	if !h.auth.hasAuth() {
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateError,
			Summary: sdk.Summary{
				Title:    "Confluence",
				Value:    "No auth",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityWarning,
				IconHint: "doc",
			},
			Alerts: []sdk.Alert{{
				ID:       "confluence-no-auth",
				Severity: sdk.SeverityWarning,
				Message:  "Connect Confluence with API token, OAuth, or cookie auth in Settings",
			}},
			RefreshAfter: 300,
			Health:       sdk.HealthAuthReq,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pages, err := h.searchContent(ctx, buildActivityCQL(h.space, h.customCQL), h.maxItems)
	if err != nil {
		sdk.Log("activity search error: %v", err)
		health := sdk.HealthDegraded
		msg := "Could not reach Confluence: " + err.Error()
		refresh := 120
		if apiErr, ok := err.(*apiHTTPError); ok {
			health = httphealth.ClassifyHTTPStatus(apiErr.StatusCode)
			refresh = httphealth.DefaultRefreshAfter(health, apiErr.RetryAfter)
			if health == httphealth.HealthAuthReq {
				msg = "Confluence authentication failed — reconnect in Settings"
			} else if health == httphealth.HealthRateLimited {
				msg = "Confluence API rate limited"
			}
		}
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateDegraded,
			Summary: sdk.Summary{
				Title:    "Confluence",
				Value:    "Err",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityWarning,
				IconHint: "doc",
			},
			Alerts:       []sdk.Alert{{ID: "confluence-fetch", Severity: sdk.SeverityWarning, Message: msg}},
			RefreshAfter: refresh,
			Health:       health,
		}
	}

	boostSortPages(pages, h.peerKeys)
	if len(pages) > h.maxItems {
		pages = pages[:h.maxItems]
	}

	h.emitDeltas(pages)

	items := make([]sdk.Item, 0, len(pages))
	for _, p := range pages {
		items = append(items, h.pageToItem(p))
	}

	value := "Quiet"
	if n := len(pages); n == 1 {
		value = "1 update"
	} else if n > 1 {
		value = fmt.Sprintf("%d updates", n)
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateReady,
		Summary: sdk.Summary{
			Title:    "Confluence",
			Value:    value,
			Trend:    sdk.TrendSteady,
			Severity: sdk.SeverityInfo,
			IconHint: "doc",
		},
		Items:        items,
		Actions:      []sdk.Action{{ID: "refresh", Label: "Refresh"}},
		Alerts:       []sdk.Alert{},
		RefreshAfter: 120,
		Health:       sdk.HealthOK,
	}
}

func (h *handler) pageToItem(p confluencePage) sdk.Item {
	subtitle := p.SpaceKey
	if p.LastUpdater != "" {
		if subtitle != "" {
			subtitle += " · "
		}
		subtitle += p.LastUpdater
	}
	if score := pageCorrelationScore(p.RelatedIssueKeys, h.peerKeys); score > 0 && len(p.RelatedIssueKeys) > 0 {
		linked := p.RelatedIssueKeys[0]
		for _, k := range p.RelatedIssueKeys {
			if _, ok := h.peerKeys[k]; ok {
				linked = k
				break
			}
		}
		subtitle += " · linked to " + linked
	}

	title := p.Title
	if p.SpaceKey != "" {
		title = fmt.Sprintf("[%s] %s", p.SpaceKey, p.Title)
	}

	detail := p.Excerpt
	if detail == "" && p.Type != "" {
		detail = p.Type
	}

	return sdk.Item{
		ID:        p.ID,
		Title:     title,
		Subtitle:  subtitle,
		Detail:    detail,
		Severity:  sdk.SeverityInfo,
		Timestamp: p.LastModified,
		DeepLink:  p.WebURL,
		Actions:   []sdk.Action{{ID: "open", Label: "Open Page"}},
		Metadata: map[string]string{
			"pageId":           p.ID,
			"spaceKey":         p.SpaceKey,
			"title":            p.Title,
			"url":              p.WebURL,
			"contentType":      p.Type,
			"version":          strconv.Itoa(p.Version),
			"labels":           strings.Join(p.Labels, ","),
			"relatedIssueKeys": joinKeys(p.RelatedIssueKeys),
		},
	}
}

func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
	if h.domain == "" {
		return false, "no Confluence domain configured"
	}
	if !h.auth.hasAuth() {
		return false, "no Confluence auth configured"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch id {
	case "searchDocs":
		query := strings.TrimSpace(params["query"])
		if query == "" {
			return false, "missing query payload"
		}
		return h.actionSearchDocs(ctx, query)
	case "getPageDetails":
		pageID := strings.TrimSpace(params["pageId"])
		if pageID == "" {
			return false, "missing pageId payload"
		}
		return h.actionGetPageDetails(ctx, pageID)
	case "getPageByUrl":
		rawURL := strings.TrimSpace(params["url"])
		if rawURL == "" {
			return false, "missing url payload"
		}
		pageID := parsePageIDFromURL(rawURL)
		if pageID == "" {
			return false, "could not parse Confluence page id from url"
		}
		return h.actionGetPageDetails(ctx, pageID)
	case "refresh", "open":
		return true, ""
	default:
		return false, "unknown action: " + id
	}
}

func (h *handler) Shutdown() {}

func (h *handler) actionSearchDocs(ctx context.Context, query string) (bool, string) {
	pages, err := h.searchContent(ctx, buildSearchCQL(query, h.space), h.maxItems)
	if err != nil {
		return false, err.Error()
	}
	if len(pages) == 0 {
		return true, "No pages found."
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Found %d page(s):", len(pages)))
	for _, p := range pages {
		line := fmt.Sprintf("  - [%s] %s", p.SpaceKey, p.Title)
		if len(p.RelatedIssueKeys) > 0 {
			line += " | issues: " + joinKeys(p.RelatedIssueKeys)
		}
		if p.WebURL != "" {
			line += " | " + p.WebURL
		}
		lines = append(lines, line)
	}
	return true, strings.Join(lines, "\n")
}

func (h *handler) actionGetPageDetails(ctx context.Context, pageID string) (bool, string) {
	page, err := h.fetchPage(ctx, pageID)
	if err != nil {
		return false, err.Error()
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Page: [%s] %s", page.SpaceKey, page.Title))
	lines = append(lines, fmt.Sprintf("Type: %s | Version: %d | Updated: %s", page.Type, page.Version, page.LastModified))
	if page.LastUpdater != "" {
		lines = append(lines, "Last updater: "+page.LastUpdater)
	}
	if len(page.Labels) > 0 {
		lines = append(lines, "Labels: "+strings.Join(page.Labels, ", "))
	}
	if len(page.RelatedIssueKeys) > 0 {
		lines = append(lines, "Related issues: "+joinKeys(page.RelatedIssueKeys))
	}
	if page.Excerpt != "" {
		lines = append(lines, "Excerpt: "+page.Excerpt)
	}
	if page.WebURL != "" {
		lines = append(lines, "Link: "+page.WebURL)
	}
	return true, strings.Join(lines, "\n")
}

// --- HTTP / API ---

type apiHTTPError struct {
	StatusCode int
	RetryAfter int
	Body       string
}

func (e *apiHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func (h *handler) resolveAPIBase(ctx context.Context) (string, error) {
	if h.auth.Kind == "oauth" && h.auth.AccessToken != "" {
		if h.cloudID == "" {
			id, err := h.resolveCloudID(ctx)
			if err != nil {
				return "", err
			}
			h.cloudID = id
		}
		base := oauthAPIBase(h.cloudID)
		if base == "" {
			return "", fmt.Errorf("missing Atlassian cloud id")
		}
		return base, nil
	}
	base := apiBase(h.domain)
	if base == "" {
		return "", fmt.Errorf("missing Confluence domain")
	}
	return base, nil
}

func (h *handler) resolveCloudID(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.atlassian.com/oauth/token/accessible-resources", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.auth.AccessToken)
	req.Header.Set("User-Agent", "smuler-confluence-plugin/"+pluginVersion)

	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", &apiHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       strings.TrimSpace(string(body)),
		}
	}

	var resources []struct {
		ID   string   `json:"id"`
		URL  string   `json:"url"`
		Name string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.Unmarshal(body, &resources); err != nil {
		return "", fmt.Errorf("parse accessible-resources: %w", err)
	}
	if len(resources) == 0 {
		return "", fmt.Errorf("no Atlassian cloud resources for this OAuth token")
	}

	want := normalizeDomain(h.domain)
	for _, r := range resources {
		if normalizeDomain(r.URL) == want {
			return r.ID, nil
		}
	}
	// Fall back to first resource when domain does not match exactly.
	return resources[0].ID, nil
}

func (h *handler) doGET(ctx context.Context, apiURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "smuler-confluence-plugin/"+pluginVersion)
	h.auth.apply(req)

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &apiHTTPError{
			StatusCode: resp.StatusCode,
			RetryAfter: httphealth.ParseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       truncate(strings.TrimSpace(string(body)), 512),
		}
	}
	return body, nil
}

type contentSearchResponse struct {
	Results []contentResult `json:"results"`
	Size    int             `json:"size"`
}

type contentResult struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title"`
	Excerpt string `json:"excerpt"`
	Space   *struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"space"`
	Version *struct {
		Number    int    `json:"number"`
		When      string `json:"when"`
		By        *struct {
			DisplayName string `json:"displayName"`
		} `json:"by"`
	} `json:"version"`
	History *struct {
		LastUpdated *struct {
			When string `json:"when"`
			By   *struct {
				DisplayName string `json:"displayName"`
			} `json:"by"`
		} `json:"lastUpdated"`
	} `json:"history"`
	Metadata *struct {
		Labels *struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"labels"`
	} `json:"metadata"`
	Links struct {
		WebUI  string `json:"webui"`
		TinyUI string `json:"tinyui"`
	} `json:"_links"`
	Body *struct {
		Storage *struct {
			Value string `json:"value"`
		} `json:"storage"`
		View *struct {
			Value string `json:"value"`
		} `json:"view"`
	} `json:"body"`
}

func (h *handler) searchContent(ctx context.Context, cql string, limit int) ([]confluencePage, error) {
	base, err := h.resolveAPIBase(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("cql", cql)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("expand", "history.lastUpdated,space,version,metadata.labels")
	apiURL := base + "/content/search?" + q.Encode()

	body, err := h.doGET(ctx, apiURL)
	if err != nil {
		return nil, err
	}
	var resp contentSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}
	pages := make([]confluencePage, 0, len(resp.Results))
	for _, r := range resp.Results {
		pages = append(pages, h.mapResult(r))
	}
	return pages, nil
}

func (h *handler) fetchPage(ctx context.Context, pageID string) (confluencePage, error) {
	base, err := h.resolveAPIBase(ctx)
	if err != nil {
		return confluencePage{}, err
	}
	q := url.Values{}
	q.Set("expand", "body.storage,history.lastUpdated,space,version,metadata.labels")
	apiURL := base + "/content/" + url.PathEscape(pageID) + "?" + q.Encode()
	body, err := h.doGET(ctx, apiURL)
	if err != nil {
		return confluencePage{}, err
	}
	var r contentResult
	if err := json.Unmarshal(body, &r); err != nil {
		return confluencePage{}, fmt.Errorf("parse page response: %w", err)
	}
	return h.mapResult(r), nil
}

func (h *handler) mapResult(r contentResult) confluencePage {
	p := confluencePage{
		ID:    r.ID,
		Title: r.Title,
		Type:  r.Type,
	}
	if r.Space != nil {
		p.SpaceKey = r.Space.Key
		p.SpaceName = r.Space.Name
	}
	if r.Version != nil {
		p.Version = r.Version.Number
		if r.Version.When != "" {
			p.LastModified = r.Version.When
		}
		if r.Version.By != nil {
			p.LastUpdater = r.Version.By.DisplayName
		}
	}
	if r.History != nil && r.History.LastUpdated != nil {
		if r.History.LastUpdated.When != "" {
			p.LastModified = r.History.LastUpdated.When
		}
		if r.History.LastUpdated.By != nil && r.History.LastUpdated.By.DisplayName != "" {
			p.LastUpdater = r.History.LastUpdated.By.DisplayName
		}
	}
	if r.Metadata != nil && r.Metadata.Labels != nil {
		for _, l := range r.Metadata.Labels.Results {
			if l.Name != "" {
				p.Labels = append(p.Labels, l.Name)
			}
		}
	}
	p.Excerpt = strings.TrimSpace(stripHTML(r.Excerpt))
	bodyText := ""
	if r.Body != nil {
		if r.Body.Storage != nil {
			bodyText = stripHTML(r.Body.Storage.Value)
		} else if r.Body.View != nil {
			bodyText = stripHTML(r.Body.View.Value)
		}
	}
	if p.Excerpt == "" {
		p.Excerpt = truncate(strings.TrimSpace(bodyText), 200)
	}
	p.WebURL = absoluteWebURL(h.domain, r.Links.WebUI)
	p.RelatedIssueKeys = extractIssueKeys(p.Title, p.Excerpt, bodyText, strings.Join(p.Labels, " "))
	return p
}

func (h *handler) emitDeltas(pages []confluencePage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	current := make(map[string]prevPageInfo, len(pages))
	for _, p := range pages {
		current[p.ID] = prevPageInfo{
			title:   p.Title,
			url:     p.WebURL,
			version: p.Version,
			space:   p.SpaceKey,
			keys:    joinKeys(p.RelatedIssueKeys),
		}
	}

	if !h.prevReady {
		h.prevPages = current
		h.prevReady = true
		return
	}

	for id, info := range current {
		prev, ok := h.prevPages[id]
		data := map[string]string{
			"pageId":           id,
			"spaceKey":         info.space,
			"url":              info.url,
			"title":            info.title,
			"relatedIssueKeys": info.keys,
		}
		if !ok {
			sdk.Emit(sdk.Event{
				Type:     "page.created",
				PluginID: pluginID,
				Message:  fmt.Sprintf("New page: %s", info.title),
				Severity: sdk.SeverityInfo,
				Data:     data,
			})
			continue
		}
		if info.version > 0 && prev.version > 0 && info.version > prev.version {
			sdk.Emit(sdk.Event{
				Type:     "page.updated",
				PluginID: pluginID,
				Message:  fmt.Sprintf("Updated: %s", info.title),
				Severity: sdk.SeverityInfo,
				Data:     data,
			})
		}
	}

	h.prevPages = current
}

func stripHTML(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := strings.ReplaceAll(b.String(), "&nbsp;", " ")
	out = strings.ReplaceAll(out, "&amp;", "&")
	out = strings.ReplaceAll(out, "&lt;", "<")
	out = strings.ReplaceAll(out, "&gt;", ">")
	out = strings.ReplaceAll(out, "&quot;", `"`)
	return strings.Join(strings.Fields(out), " ")
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func main() {
	sdk.Run(pluginID, pluginVersion, &handler{
		client:    &http.Client{Timeout: 30 * time.Second},
		prevPages: make(map[string]prevPageInfo),
		peerKeys:  make(map[string]struct{}),
	})
}
