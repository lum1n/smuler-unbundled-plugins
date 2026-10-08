package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	issueKeyPattern   = regexp.MustCompile(`\b([A-Z][A-Z0-9_]+-\d+)\b`)
	pageIDFromURL     = regexp.MustCompile(`(?:/pages/(\d+)|[?&]pageId=(\d+))`)
	digitsOnlyPageID  = regexp.MustCompile(`\d{5,}`)
	voiceSearchPrefix = regexp.MustCompile(`(?i)^\s*(?:(?:please\s+)?(?:search|find|look\s*up|show|get|open)\s+)?(?:(?:in\s+|on\s+|from\s+)?(?:confluence|docs?|documentation)\s+)?(?:(?:for|about|regarding)\s+)?(.+?)\s*$`)
)

type confluenceAuth struct {
	Kind         string
	APIKey       string
	AccessToken  string
	CookieHeader string
}

func (a confluenceAuth) hasAuth() bool {
	return a.APIKey != "" || a.AccessToken != "" || a.CookieHeader != ""
}

func (a confluenceAuth) apply(req *http.Request) {
	// Honor the selected kind first: when several providers are connected,
	// resolveAPIBase routes by Kind (OAuth → api.atlassian.com gateway), so
	// sending Basic credentials there would always 401.
	switch a.Kind {
	case "oauth":
		if a.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+a.AccessToken)
			return
		}
	case "browser_import":
		if a.CookieHeader != "" {
			req.Header.Set("Cookie", a.CookieHeader)
			return
		}
	}
	switch {
	case a.APIKey != "":
		parts := strings.SplitN(a.APIKey, ":", 2)
		user, pass := parts[0], ""
		if len(parts) == 2 {
			pass = parts[1]
		}
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req.Header.Set("Authorization", "Basic "+token)
	case a.AccessToken != "":
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	case a.CookieHeader != "":
		req.Header.Set("Cookie", a.CookieHeader)
	}
}

func normalizeDomain(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/")
}

func isCloudDomain(domain string) bool {
	u, err := url.Parse(domain)
	if err != nil {
		return strings.Contains(domain, "atlassian.net")
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, "atlassian.net")
}

// apiBase returns the REST API root for cookie/basic auth requests.
// Cloud: {domain}/wiki/rest/api
// Server/DC: {domain}/rest/api
func apiBase(domain string) string {
	domain = normalizeDomain(domain)
	if domain == "" {
		return ""
	}
	if isCloudDomain(domain) {
		return domain + "/wiki/rest/api"
	}
	return domain + "/rest/api"
}

// oauthAPIBase returns the Atlassian gateway API root for a cloud ID.
func oauthAPIBase(cloudID string) string {
	cloudID = strings.TrimSpace(cloudID)
	if cloudID == "" {
		return ""
	}
	return "https://api.atlassian.com/ex/confluence/" + cloudID + "/wiki/rest/api"
}

func webBase(domain string) string {
	domain = normalizeDomain(domain)
	if domain == "" {
		return ""
	}
	if isCloudDomain(domain) {
		return domain + "/wiki"
	}
	return domain
}

func buildActivityCQL(space, custom string) string {
	custom = strings.TrimSpace(custom)
	if custom != "" {
		lower := strings.ToLower(custom)
		if strings.Contains(lower, "order by") {
			return custom
		}
		return custom + " order by lastmodified desc"
	}
	cql := "type in (page,blogpost)"
	if space = strings.TrimSpace(space); space != "" {
		cql = fmt.Sprintf("space = %s AND %s", quoteCQLValue(space), cql)
	}
	return cql + " order by lastmodified desc"
}

func buildSearchCQL(query, space string) string {
	query = strings.TrimSpace(query)
	escaped := escapeCQLString(query)
	// Prefer siteSearch (Confluence quick-find semantics) so companion/voice
	// queries match pages outside the recent-activity snapshot.
	cql := fmt.Sprintf(`type in (page,blogpost) AND (siteSearch ~ "%s" OR title ~ "%s" OR text ~ "%s")`, escaped, escaped, escaped)
	if space = strings.TrimSpace(space); space != "" {
		cql = fmt.Sprintf("space = %s AND %s", quoteCQLValue(space), cql)
	}
	return cql + " order by lastmodified desc"
}

func quoteCQLValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return `""`
	}
	// Unquoted bare keys are fine for simple space keys; quote if needed.
	if regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`).MatchString(v) {
		return v
	}
	return `"` + escapeCQLString(v) + `"`
}

func escapeCQLString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func extractIssueKeys(texts ...string) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, t := range texts {
		for _, m := range issueKeyPattern.FindAllString(t, -1) {
			if _, ok := seen[m]; ok {
				continue
			}
			seen[m] = struct{}{}
			keys = append(keys, m)
		}
	}
	sort.Strings(keys)
	return keys
}

func parsePageIDFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	m := pageIDFromURL.FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

func collectPeerIssueKeys(snapshots []peerSnapshotLite) map[string]struct{} {
	keys := map[string]struct{}{}
	for _, snap := range snapshots {
		if snap.PluginID != "" && snap.PluginID != "jira" && snap.PluginID != "linear" {
			// Still scan titles for issue-key shapes from any peer.
		}
		for _, item := range snap.Items {
			for _, k := range extractIssueKeys(item.ID, item.Title, item.Subtitle, item.Detail) {
				keys[k] = struct{}{}
			}
			if item.Metadata != nil {
				if rel := item.Metadata["relatedIssueKeys"]; rel != "" {
					for _, part := range strings.Split(rel, ",") {
						part = strings.TrimSpace(part)
						if part != "" {
							keys[part] = struct{}{}
						}
					}
				}
				if ik := item.Metadata["issueKey"]; ik != "" {
					keys[strings.TrimSpace(ik)] = struct{}{}
				}
			}
		}
	}
	return keys
}

type peerSnapshotLite struct {
	PluginID string
	Items    []peerItemLite
}

type peerItemLite struct {
	ID       string
	Title    string
	Subtitle string
	Detail   string
	Metadata map[string]string
}

func pageCorrelationScore(relatedKeys []string, peerKeys map[string]struct{}) int {
	if len(peerKeys) == 0 || len(relatedKeys) == 0 {
		return 0
	}
	score := 0
	for _, k := range relatedKeys {
		if _, ok := peerKeys[k]; ok {
			score++
		}
	}
	return score
}

func boostSortPages(pages []confluencePage, peerKeys map[string]struct{}) {
	sort.SliceStable(pages, func(i, j int) bool {
		si := pageCorrelationScore(pages[i].RelatedIssueKeys, peerKeys)
		sj := pageCorrelationScore(pages[j].RelatedIssueKeys, peerKeys)
		if si != sj {
			return si > sj
		}
		// Prefer more recently updated when scores tie (ISO8601 lexicographic works for RFC3339).
		return pages[i].LastModified > pages[j].LastModified
	})
}

func parseMaxItems(raw string, def int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return def
	}
	if n > 50 {
		return 50
	}
	return n
}

func parseBool(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func absoluteWebURL(domain, webui string) string {
	webui = strings.TrimSpace(webui)
	if webui == "" {
		return ""
	}
	if strings.HasPrefix(webui, "http://") || strings.HasPrefix(webui, "https://") {
		return webui
	}
	base := webBase(domain)
	if !strings.HasPrefix(webui, "/") {
		webui = "/" + webui
	}
	return base + webui
}

func joinKeys(keys []string) string {
	return strings.Join(keys, ",")
}

func firstNonEmpty(params map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(params[k]); v != "" {
			return v
		}
	}
	return ""
}

// normalizeSearchQuery strips common companion/voice prefixes so CQL search
// uses the document topic rather than the whole utterance.
func normalizeSearchQuery(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if m := voiceSearchPrefix.FindStringSubmatch(raw); len(m) == 2 {
		trimmed := strings.TrimSpace(m[1])
		// Avoid collapsing utterances that are only the product name.
		lower := strings.ToLower(trimmed)
		if trimmed != "" && lower != "confluence" && lower != "docs" && lower != "documentation" && lower != "doc" {
			return trimmed
		}
	}
	return raw
}

// normalizeTimestamp converts Confluence timestamps ("2024-01-01T10:00:00.000+01:00")
// to RFC3339 UTC so the host parses them and lexicographic sorting is correct
// across offsets. Unparseable values are passed through unchanged.
func normalizeTimestamp(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return raw
}
