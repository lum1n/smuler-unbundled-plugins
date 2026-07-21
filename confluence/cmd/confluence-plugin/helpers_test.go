package main

import (
	"net/http"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"https://acme.atlassian.net", "https://acme.atlassian.net"},
		{"https://acme.atlassian.net/", "https://acme.atlassian.net"},
		{"acme.atlassian.net", "https://acme.atlassian.net"},
		{"https://wiki.example.com/wiki", "https://wiki.example.com"},
	}
	for _, tc := range cases {
		if got := normalizeDomain(tc.in); got != tc.want {
			t.Errorf("normalizeDomain(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestAPIBase(t *testing.T) {
	if got := apiBase("https://acme.atlassian.net"); got != "https://acme.atlassian.net/wiki/rest/api" {
		t.Errorf("cloud apiBase=%q", got)
	}
	if got := apiBase("https://confluence.example.com"); got != "https://confluence.example.com/rest/api" {
		t.Errorf("server apiBase=%q", got)
	}
	if got := oauthAPIBase("cloud-123"); got != "https://api.atlassian.com/ex/confluence/cloud-123/wiki/rest/api" {
		t.Errorf("oauthAPIBase=%q", got)
	}
}

func TestBuildActivityCQL(t *testing.T) {
	got := buildActivityCQL("", "")
	want := "type in (page,blogpost) order by lastmodified desc"
	if got != want {
		t.Errorf("default CQL=%q want %q", got, want)
	}
	got = buildActivityCQL("ENG", "")
	want = "space = ENG AND type in (page,blogpost) order by lastmodified desc"
	if got != want {
		t.Errorf("space CQL=%q want %q", got, want)
	}
	got = buildActivityCQL("ENG", "label = handbook")
	want = "label = handbook order by lastmodified desc"
	if got != want {
		t.Errorf("custom CQL=%q want %q", got, want)
	}
	got = buildActivityCQL("", "type = page order by created desc")
	want = "type = page order by created desc"
	if got != want {
		t.Errorf("custom with order=%q want %q", got, want)
	}
}

func TestBuildSearchCQL(t *testing.T) {
	got := buildSearchCQL(`foo "bar"`, "ENG")
	if !containsAll(got, `space = ENG`, `title ~ "foo \"bar\""`, `text ~ "foo \"bar\""`, "order by lastmodified desc") {
		t.Errorf("search CQL unexpected: %q", got)
	}
}

func TestExtractIssueKeys(t *testing.T) {
	keys := extractIssueKeys("See PROJ-123 and ENG-9 in docs", "Also PROJ-123 again")
	if len(keys) != 2 || keys[0] != "ENG-9" || keys[1] != "PROJ-123" {
		t.Errorf("keys=%v", keys)
	}
}

func TestParsePageIDFromURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://acme.atlassian.net/wiki/spaces/ENG/pages/123456789/Title", "123456789"},
		{"https://wiki.example.com/pages/viewpage.action?pageId=98765", "98765"},
		{"https://example.com/no-page", ""},
	}
	for _, tc := range cases {
		if got := parsePageIDFromURL(tc.in); got != tc.want {
			t.Errorf("parsePageIDFromURL(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestAuthApply(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	a := confluenceAuth{Kind: "api_key", APIKey: "user:token"}
	a.apply(req)
	if got := req.Header.Get("Authorization"); got == "" || got[:6] != "Basic " {
		t.Errorf("basic auth header=%q", got)
	}

	req, _ = http.NewRequest("GET", "https://example.com", nil)
	a = confluenceAuth{Kind: "oauth", AccessToken: "tok"}
	a.apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("bearer=%q", got)
	}

	req, _ = http.NewRequest("GET", "https://example.com", nil)
	a = confluenceAuth{Kind: "browser_import", CookieHeader: "sid=abc"}
	a.apply(req)
	if got := req.Header.Get("Cookie"); got != "sid=abc" {
		t.Errorf("cookie=%q", got)
	}
}

func TestCollectPeerIssueKeysAndBoost(t *testing.T) {
	peers := []peerSnapshotLite{{
		PluginID: "jira",
		Items: []peerItemLite{
			{ID: "PROJ-123", Title: "PROJ-123 Fix login"},
			{ID: "other", Title: "Something", Metadata: map[string]string{"issueKey": "ENG-7"}},
		},
	}}
	keys := collectPeerIssueKeys(peers)
	if _, ok := keys["PROJ-123"]; !ok {
		t.Fatal("missing PROJ-123")
	}
	if _, ok := keys["ENG-7"]; !ok {
		t.Fatal("missing ENG-7")
	}

	pages := []confluencePage{
		{ID: "1", Title: "Unrelated", LastModified: "2024-02-01T00:00:00.000Z"},
		{ID: "2", Title: "Linked", RelatedIssueKeys: []string{"PROJ-123"}, LastModified: "2024-01-01T00:00:00.000Z"},
	}
	boostSortPages(pages, keys)
	if pages[0].ID != "2" {
		t.Errorf("expected correlated page first, got %#v", pages)
	}
}

func TestAbsoluteWebURL(t *testing.T) {
	got := absoluteWebURL("https://acme.atlassian.net", "/spaces/ENG/pages/1")
	want := "https://acme.atlassian.net/wiki/spaces/ENG/pages/1"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	got = absoluteWebURL("https://wiki.example.com", "/display/ENG/Page")
	want = "https://wiki.example.com/display/ENG/Page"
	if got != want {
		t.Errorf("server got %q want %q", got, want)
	}
}

func TestParseMaxItems(t *testing.T) {
	if parseMaxItems("", 10) != 10 {
		t.Fatal("default")
	}
	if parseMaxItems("25", 10) != 25 {
		t.Fatal("25")
	}
	if parseMaxItems("999", 10) != 50 {
		t.Fatal("cap")
	}
}

func TestNormalizeSearchQuery(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"onboarding checklist", "onboarding checklist"},
		{"search confluence for onboarding checklist", "onboarding checklist"},
		{"find docs about SSO setup", "SSO setup"},
		{"look up documentation for vacation policy", "vacation policy"},
		{"confluence", "confluence"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeSearchQuery(tc.in); got != tc.want {
			t.Errorf("normalizeSearchQuery(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	got := firstNonEmpty(map[string]string{"q": "hello", "query": ""}, "query", "q", "text")
	if got != "hello" {
		t.Errorf("got %q", got)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
