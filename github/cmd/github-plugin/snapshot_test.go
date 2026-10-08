package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

func TestBuildSnapshotHTTPStatuses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantHealth string
		wantValue  string
	}{
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{}`,
			wantHealth: "auth_required",
			wantValue:  "Auth required",
		},
		{
			name:       "rate limited",
			statusCode: http.StatusTooManyRequests,
			body:       `{}`,
			wantHealth: "rate_limited",
			wantValue:  "Rate limited",
		},
		{
			name:       "empty reviews",
			statusCode: http.StatusOK,
			body:       `{"total_count":0,"items":[]}`,
			wantHealth: "ok",
			wantValue:  "No reviews",
		},
		{
			name:       "review backlog",
			statusCode: http.StatusOK,
			body:       `{"total_count":2,"items":[{"number":1,"title":"Fix","html_url":"https://github.com/o/r/pull/1","repository_url":"https://api.github.com/repos/o/r","user":{"login":"alice"},"state":"open"}]}`,
			wantHealth: "ok",
			wantValue:  "2 reviews",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			pl := &githubPlugin{
				token:     "test-token",
				client:    srv.Client(),
				apiBase:   srv.URL,
				prevPRs:   make(map[string]prevPRInfo),
				prMetaMap: make(map[string]prMeta),
			}

			snap := pl.buildSnapshot()
			if snap.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", snap.Health, tc.wantHealth)
			}
			if snap.Summary.Value != tc.wantValue {
				t.Fatalf("summary value = %q, want %q", snap.Summary.Value, tc.wantValue)
			}
		})
	}
}

func TestBuildSnapshotRequiresToken(t *testing.T) {
	pl := &githubPlugin{
		prevPRs:   make(map[string]prevPRInfo),
		prMetaMap: make(map[string]prMeta),
	}
	snap := pl.buildSnapshot()
	if snap.Health != "auth_required" {
		t.Fatalf("health = %q, want auth_required", snap.Health)
	}
}

func TestResolveTokenPrefersSecretOverRecordID(t *testing.T) {
	params := sdk.InitializeParams{
		ProviderAuths: []sdk.ProviderAuthContext{{
			ProviderID:  "github",
			Kind:        "oauth",
			AccountID:   "5b0a7c1e-uuid",
			AccessToken: "gho_secret",
		}},
	}
	if got := resolveToken(params); got != "gho_secret" {
		t.Fatalf("token = %q, want access token", got)
	}

	legacy := sdk.InitializeParams{Auth: &sdk.AuthContext{AccountID: "ghp_legacy"}}
	if got := resolveToken(legacy); got != "ghp_legacy" {
		t.Fatalf("legacy token = %q", got)
	}
}

func TestBuildSnapshotForbiddenRateLimit(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	pl := &githubPlugin{token: "t", client: srv.Client(), apiBase: srv.URL, prevPRs: map[string]prevPRInfo{}, prMetaMap: map[string]prMeta{}}
	snap := pl.buildSnapshot()
	if snap.Health != "rate_limited" {
		t.Fatalf("health = %q, want rate_limited", snap.Health)
	}
	if snap.RefreshAfter < 600 {
		t.Fatalf("refreshAfter = %d, want >= 600", snap.RefreshAfter)
	}
}

func TestBuildSnapshotUniqueIDsAcrossRepos(t *testing.T) {
	body := `{"total_count":2,"items":[
		{"id":101,"number":1,"title":"A","html_url":"https://github.com/o/a/pull/1","repository_url":"https://api.github.com/repos/o/a","user":{"login":"x"}},
		{"id":202,"number":1,"title":"B","html_url":"https://github.com/o/b/pull/1","repository_url":"https://api.github.com/repos/o/b","user":{"login":"y"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "per_page=50") {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	pl := &githubPlugin{token: "t", client: srv.Client(), apiBase: srv.URL, prevPRs: map[string]prevPRInfo{}, prMetaMap: map[string]prMeta{}}
	snap := pl.buildSnapshot()
	if len(snap.Items) != 2 || snap.Items[0].ID == snap.Items[1].ID {
		t.Fatalf("expected two distinct items, got %+v", snap.Items)
	}
	if snap.Items[1].Subtitle != "o/b" {
		t.Fatalf("subtitle = %q, want o/b", snap.Items[1].Subtitle)
	}
	if pl.prMetaMap["pr-202"].repo != "o/b" {
		t.Fatalf("meta for pr-202 = %+v", pl.prMetaMap["pr-202"])
	}
	if !pl.seeded {
		t.Fatalf("expected delta state to be seeded after first success")
	}
}
