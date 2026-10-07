package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ts(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

func newTestPlugin(srv *httptest.Server) *ciPlugin {
	return &ciPlugin{
		token:     "t",
		client:    srv.Client(),
		apiBase:   srv.URL,
		prevFails: map[string]prevFailInfo{},
		etags:     map[string]cachedResponse{},
	}
}

func TestFailingRunsUsesLatestRunPerWorkflow(t *testing.T) {
	runs := []ghWorkflowRun{
		{ID: "3", WorkflowID: "1", HeadBranch: "main", Status: "completed", Conclusion: "success", CreatedAt: ts(time.Hour)},
		{ID: "2", WorkflowID: "1", HeadBranch: "main", Status: "completed", Conclusion: "failure", CreatedAt: ts(2 * time.Hour)},
		{ID: "5", WorkflowID: "2", HeadBranch: "main", Status: "in_progress", CreatedAt: ts(time.Minute)},
		{ID: "4", WorkflowID: "2", HeadBranch: "main", Status: "completed", Conclusion: "failure", CreatedAt: ts(3 * time.Hour)},
		{ID: "9", WorkflowID: "3", HeadBranch: "main", Status: "completed", Conclusion: "failure", CreatedAt: ts(30 * 24 * time.Hour)},
	}
	for i := range runs {
		runs[i].Repository.FullName = "o/r"
	}
	got := failingRuns(runs, time.Now())
	if len(got) != 1 || got[0].ID.String() != "4" {
		t.Fatalf("failing = %+v, want only run 4", got)
	}
}

func TestGetStatusEndToEndWithETag(t *testing.T) {
	var runsCalls, notModified int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user/repos":
			fmt.Fprintf(w, `[{"full_name":"o/active","pushed_at":%q},{"full_name":"o/dormant","pushed_at":%q}]`, ts(time.Hour), ts(90*24*time.Hour))
		case r.URL.Path == "/repos/o/active/actions/runs":
			atomic.AddInt32(&runsCalls, 1)
			if r.Header.Get("If-None-Match") == `"v1"` {
				atomic.AddInt32(&notModified, 1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			fmt.Fprintf(w, `{"workflow_runs":[{"id":7,"workflow_id":1,"name":"CI","status":"completed","conclusion":"failure","created_at":%q,"head_branch":"main","repository":{"full_name":"o/active"}}]}`, ts(time.Hour))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := newTestPlugin(srv)
	for i := 0; i < 2; i++ {
		snap := p.GetStatus()
		if snap.Health != "ok" || snap.Summary.Value != "1 failing" || len(snap.Items) != 1 {
			t.Fatalf("iteration %d: snapshot = %+v", i, snap)
		}
	}
	if runsCalls != 2 || notModified != 1 {
		t.Fatalf("runsCalls=%d notModified=%d, want 2/1", runsCalls, notModified)
	}
}

func TestGetStatusAuthAndRateLimit(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		want   string
	}{
		{"unauthorized", http.StatusUnauthorized, nil, "auth_required"},
		{"primary rate limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0"}, "rate_limited"},
		{"secondary rate limit", http.StatusTooManyRequests, map[string]string{"Retry-After": "120"}, "rate_limited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"secret-ish body"}`))
			}))
			defer srv.Close()
			snap := newTestPlugin(srv).GetStatus()
			if snap.Health != tc.want {
				t.Fatalf("health = %q, want %q", snap.Health, tc.want)
			}
			if strings.Contains(snap.Summary.Value, "secret-ish") {
				t.Fatalf("response body leaked into summary: %q", snap.Summary.Value)
			}
		})
	}
}

func TestGetStatusPartialFailureIsNotAllPassing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user/repos":
			fmt.Fprintf(w, `[{"full_name":"o/a","pushed_at":%q},{"full_name":"o/b","pushed_at":%q}]`, ts(time.Hour), ts(time.Hour))
		case "/repos/o/a/actions/runs":
			_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	snap := newTestPlugin(srv).GetStatus()
	if snap.Health != "degraded" {
		t.Fatalf("health = %q, want degraded", snap.Health)
	}
}
