package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
				token:   "test-token",
				client:  srv.Client(),
				apiBase: srv.URL,
				prevPRs: make(map[string]prevPRInfo),
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
