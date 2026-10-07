package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

func newTestHandler(t *testing.T, handler http.HandlerFunc, cfg linearConfig) *linearHandler {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &linearHandler{
		token:        "lin_api_test",
		config:       cfg,
		client:       srv.Client(),
		apiURL:       srv.URL,
		prevIssueIDs: map[string]prevIssueInfo{},
	}
}

func TestResolveAuthHeader(t *testing.T) {
	cases := []struct {
		name   string
		params sdk.InitializeParams
		want   string
	}{
		{"oauth provider auth uses bearer", sdk.InitializeParams{ProviderAuths: []sdk.ProviderAuthContext{{AccountID: "uuid", AccessToken: "tok"}}}, "Bearer tok"},
		{"api key provider auth is bare", sdk.InitializeParams{ProviderAuths: []sdk.ProviderAuthContext{{AccountID: "uuid", APIKey: "lin_api_x"}}}, "lin_api_x"},
		{"legacy api key is bare", sdk.InitializeParams{Auth: &sdk.AuthContext{AccountID: " lin_api_y "}}, "lin_api_y"},
		{"legacy oauth token gets bearer", sdk.InitializeParams{Auth: &sdk.AuthContext{AccountID: "lin_oauth_z"}}, "Bearer lin_oauth_z"},
	}
	for _, tc := range cases {
		if got := resolveAuthHeader(tc.params); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestPriorityMapping(t *testing.T) {
	now := time.Now()
	sev, detail, _ := issueUrgency(issueNode{Priority: 1}, now)
	if sev != "warning" || !strings.Contains(detail, "Urgent") {
		t.Fatalf("urgent: sev=%q detail=%q", sev, detail)
	}
	sev, _, _ = issueUrgency(issueNode{Priority: 4}, now)
	if sev != "info" {
		t.Fatalf("low priority should be info, got %q", sev)
	}
}

func TestBuildSnapshotFiltersMentionsAndNormalizesTimestamps(t *testing.T) {
	var gotQuery string
	h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "lin_api_test" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		_ = json.Unmarshal(body, &req)
		gotQuery = req.Query
		_, _ = w.Write([]byte(`{"data":{
			"viewer":{"assignedIssues":{"nodes":[{"id":"i1","identifier":"ENG-1","title":"A","url":"https://linear.app/x/issue/ENG-1","updatedAt":"2024-05-01T10:00:00.123Z","priority":0,"state":{"name":"Todo","type":"unstarted"},"team":{"name":"Eng","key":"ENG"}}]}},
			"notifications":{"nodes":[
				{"id":"n1","type":"issueMention","updatedAt":"2024-05-01T10:00:00.000Z","readAt":null,"issue":{"id":"i2","identifier":"ENG-2","title":"B","url":"u","updatedAt":"2024-05-01T10:00:00.000Z"}},
				{"id":"n2","type":"issueMention","updatedAt":"2024-05-01T10:00:00.000Z","readAt":"2024-05-01T11:00:00.000Z","issue":{"id":"i3","identifier":"ENG-3","title":"C","url":"u"}},
				{"id":"n3","type":"issueStatusChanged","updatedAt":"2024-05-01T10:00:00.000Z","readAt":null,"issue":{"id":"i4","identifier":"ENG-4","title":"D","url":"u"}},
				{"id":"n4","type":"projectUpdateCreated","updatedAt":"2024-05-01T10:00:00.000Z","readAt":null}
			]}}}`))
	}, linearConfig{ShowAssigned: true, ShowMentions: true})

	snap := h.buildSnapshot()
	if snap.Health != "ok" {
		t.Fatalf("health = %q", snap.Health)
	}
	if !strings.Contains(gotQuery, "... on IssueNotification") {
		t.Fatalf("query must use IssueNotification fragment: %s", gotQuery)
	}
	if len(snap.Items) != 2 {
		t.Fatalf("items = %d, want 2 (assigned + unread mention): %+v", len(snap.Items), snap.Items)
	}
	for _, it := range snap.Items {
		if it.Timestamp != "" && strings.Contains(it.Timestamp, ".") {
			t.Fatalf("timestamp not normalized: %q", it.Timestamp)
		}
	}
}

func TestBuildSnapshotGraphQLRateLimitAndAuth(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"RATELIMITED", "rate_limited"},
		{"AUTHENTICATION_ERROR", "auth_required"},
	}
	for _, tc := range cases {
		h := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"errors":[{"message":"nope","extensions":{"code":"` + tc.code + `"}}]}`))
		}, linearConfig{ShowAssigned: true})
		if snap := h.buildSnapshot(); snap.Health != tc.want {
			t.Errorf("%s: health = %q, want %q", tc.code, snap.Health, tc.want)
		}
	}
}

func TestBuildQueryNoSources(t *testing.T) {
	h := &linearHandler{config: linearConfig{}}
	if q, _ := h.buildQuery(); q != "" {
		t.Fatalf("expected empty query, got %q", q)
	}
}
