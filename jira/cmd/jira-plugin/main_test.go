package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

func newTestHandler(t *testing.T, h http.HandlerFunc) *jiraHandler {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &jiraHandler{
		client:        srv.Client(),
		config:        jiraConfig{ShowAssigned: true, MaxIssues: 10},
		domain:        srv.URL,
		cookieHeader:  "cloud.session.token=abc",
		prevIssueKeys: map[string]string{},
	}
}

func TestGetStatusCountsIssuesWithoutTotal(t *testing.T) {
	p := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Atlassian-Token") != "no-check" {
			t.Errorf("missing XSRF opt-out header")
		}
		if r.Header.Get("Cookie") != "cloud.session.token=abc" {
			t.Errorf("cookie header = %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[
			{"id":"1","key":"P-1","fields":{"summary":"A","status":{"name":"To Do"},"issuetype":{"name":"Task"},"priority":{"name":"High"},"updated":"2024-05-01T10:00:00.000+0200"}},
			{"id":"2","key":"P-2","fields":{"summary":"B","status":{"name":"To Do"},"issuetype":{"name":"Bug"},"priority":null,"updated":"2024-05-02T10:00:00.000+0000"}}
		],"isLast":false,"nextPageToken":"x"}`))
	})
	snap := p.GetStatus()
	if snap.Health != "ok" {
		t.Fatalf("health = %q", snap.Health)
	}
	if snap.Summary.Value != "2+ issues" {
		t.Fatalf("summary = %q, want 2+ issues", snap.Summary.Value)
	}
	if snap.Items[0].ID != "P-2" || snap.Items[1].Timestamp != "2024-05-01T08:00:00Z" {
		t.Fatalf("items not sorted/normalized: %+v", snap.Items)
	}
}

func TestGetStatusErrorsDoNotLeakBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ctype  string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, "application/json", "auth_required"},
		{"login page redirect", http.StatusOK, "text/html; charset=utf-8", "auth_required"},
		{"rate limited", http.StatusTooManyRequests, "application/json", "rate_limited"},
		{"server error", http.StatusInternalServerError, "application/json", "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ctype)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`<html>SENSITIVE</html>`))
			})
			snap := p.GetStatus()
			if snap.Health != tc.want {
				t.Fatalf("health = %q, want %q", snap.Health, tc.want)
			}
			for _, a := range snap.Alerts {
				if strings.Contains(a.Message, "SENSITIVE") {
					t.Fatalf("body leaked: %q", a.Message)
				}
			}
		})
	}
}

func TestBuildJQLKeepsCustomOrderBy(t *testing.T) {
	p := &jiraHandler{config: jiraConfig{JQL: "project = X order by priority DESC"}}
	if got := p.buildJQL(); strings.Count(strings.ToLower(got), "order by") != 1 {
		t.Fatalf("jql = %q", got)
	}
}

func TestIsOverdueDueTodayIsNotOverdue(t *testing.T) {
	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	if isOverdue(jiraIssue{Fields: jiraFields{DueDate: &today}}, now) {
		t.Fatalf("due today must not be overdue")
	}
	if !isOverdue(jiraIssue{Fields: jiraFields{DueDate: &yesterday}}, now) {
		t.Fatalf("due yesterday must be overdue")
	}
}

func TestInitializeAcceptsWebSessionCookie(t *testing.T) {
	p := &jiraHandler{prevIssueKeys: map[string]string{}}
	health := p.Initialize(sdk.InitializeParams{
		Config:        map[string]string{"domain": "example.atlassian.net"},
		ProviderAuths: []sdk.ProviderAuthContext{{Kind: "web_session", CookieHeader: "a=b"}},
	})
	if health != "ok" || p.cookieHeader != "a=b" || p.domain != "https://example.atlassian.net" {
		t.Fatalf("health=%q cookie=%q domain=%q", health, p.cookieHeader, p.domain)
	}
}
