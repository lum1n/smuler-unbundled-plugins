package cursorapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lum1n/smuler/plugins/cursor-cloud-agents/internal/cursorapi"
)

func TestMeSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "test-key" || pass != "" {
			t.Fatalf("unexpected auth: %v %q", ok, user)
		}
		_ = json.NewEncoder(w).Encode(cursorapi.MeResponse{
			UserEmail:  "dev@example.com",
			APIKeyName: "Dev Key",
		})
	}))
	defer srv.Close()

	client := cursorapi.NewClient("test-key", srv.Client())
	client.SetBaseURL(srv.URL)

	me, err := client.Me(context.Background())
	if err != nil {
		t.Fatalf("Me() error: %v", err)
	}
	if me.UserEmail != "dev@example.com" {
		t.Fatalf("UserEmail = %q", me.UserEmail)
	}
}

func TestListAllAgentsPagination(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		page++
		if page == 1 {
			if r.URL.Query().Get("cursor") != "" {
				t.Fatalf("first page should not include cursor")
			}
			_ = json.NewEncoder(w).Encode(cursorapi.AgentListResponse{
				Items: []cursorapi.AgentListItem{
					{ID: "bc-1", Name: "First", Status: "ACTIVE", URL: "https://cursor.com/agents/bc-1"},
				},
				NextCursor: "bc-2",
			})
			return
		}
		if r.URL.Query().Get("cursor") != "bc-2" {
			t.Fatalf("cursor = %q", r.URL.Query().Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(cursorapi.AgentListResponse{
			Items: []cursorapi.AgentListItem{
				{ID: "bc-2", Name: "Second", Status: "ACTIVE", URL: "https://cursor.com/agents/bc-2"},
			},
		})
	}))
	defer srv.Close()

	client := cursorapi.NewClient("test-key", srv.Client())
	client.SetBaseURL(srv.URL)

	agents, err := client.ListAllAgents(context.Background(), cursorapi.ListOptions{})
	if err != nil {
		t.Fatalf("ListAllAgents() error: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("len(agents) = %d, want 2", len(agents))
	}
}

func TestGetAgentRunUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/agents/bc-1":
			_ = json.NewEncoder(w).Encode(cursorapi.AgentDetail{
				AgentListItem: cursorapi.AgentListItem{
					ID: "bc-1", Name: "Agent", Status: "ACTIVE", LatestRunID: "run-1",
				},
				Repos: []cursorapi.AgentRepo{{URL: "https://github.com/org/repo", StartingRef: "main"}},
			})
		case "/v1/agents/bc-1/runs/run-1":
			_ = json.NewEncoder(w).Encode(cursorapi.RunDetail{
				ID: "run-1", AgentID: "bc-1", Status: "FINISHED",
				Result: "done",
				Git: cursorapi.RunGit{
					Branches: []cursorapi.GitBranch{{Branch: "cursor/test", PRURL: "https://github.com/org/repo/pull/1"}},
				},
			})
		case "/v1/agents/bc-1/usage":
			_ = json.NewEncoder(w).Encode(cursorapi.AgentUsageResponse{
				TotalUsage: cursorapi.TokenUsage{TotalTokens: 100},
			})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := cursorapi.NewClient("test-key", srv.Client())
	client.SetBaseURL(srv.URL)

	detail, err := client.GetAgent(context.Background(), "bc-1")
	if err != nil {
		t.Fatalf("GetAgent() error: %v", err)
	}
	if len(detail.Repos) != 1 {
		t.Fatalf("repos = %d", len(detail.Repos))
	}

	run, err := client.GetRun(context.Background(), "bc-1", "run-1")
	if err != nil {
		t.Fatalf("GetRun() error: %v", err)
	}
	if run.Result != "done" {
		t.Fatalf("result = %q", run.Result)
	}

	usage, err := client.GetUsage(context.Background(), "bc-1")
	if err != nil {
		t.Fatalf("GetUsage() error: %v", err)
	}
	if usage.TotalUsage.TotalTokens != 100 {
		t.Fatalf("tokens = %d", usage.TotalUsage.TotalTokens)
	}
}

func TestAPIErrorAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
	}))
	defer srv.Close()

	client := cursorapi.NewClient("bad-key", srv.Client())
	client.SetBaseURL(srv.URL)

	_, err := client.Me(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*cursorapi.APIError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if !apiErr.IsAuth() {
		t.Fatal("expected auth error")
	}
}

func TestMissingAPIKey(t *testing.T) {
	client := cursorapi.NewClient("", nil)
	_, err := client.Me(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "missing api key") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnrichAgentsReusesSettledAgents(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/agents/bc-1":
			_ = json.NewEncoder(w).Encode(cursorapi.AgentDetail{
				AgentListItem: cursorapi.AgentListItem{ID: "bc-1", LatestRunID: "run-1"},
			})
		case "/v1/agents/bc-1/runs/run-1":
			_ = json.NewEncoder(w).Encode(cursorapi.RunDetail{ID: "run-1", Status: "FINISHED"})
		case "/v1/agents/bc-1/usage":
			_ = json.NewEncoder(w).Encode(cursorapi.AgentUsageResponse{TotalUsage: cursorapi.TokenUsage{TotalTokens: 5}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := cursorapi.NewClient("test-key", srv.Client())
	client.SetBaseURL(srv.URL)
	agents := []cursorapi.AgentListItem{{ID: "bc-1", Status: "FINISHED", UpdatedAt: "2026-01-01T00:00:00Z", LatestRunID: "run-1"}}
	opts := cursorapi.EnrichOptions{DetailLevel: "full", MaxAgents: 10}

	first := client.EnrichAgents(context.Background(), agents, opts)
	if first[0].Run == nil || first[0].Usage == nil || first[0].Detail == nil {
		t.Fatalf("first enrichment incomplete: %+v", first[0])
	}
	if calls != 3 {
		t.Fatalf("calls after first = %d, want 3", calls)
	}

	second := client.EnrichAgents(context.Background(), agents, opts)
	if calls != 3 {
		t.Fatalf("settled agent was re-fetched: calls = %d", calls)
	}
	if second[0].Usage == nil || second[0].Usage.TotalUsage.TotalTokens != 5 {
		t.Fatalf("cached enrichment lost: %+v", second[0])
	}

	agents[0].UpdatedAt = "2026-01-02T00:00:00Z"
	client.EnrichAgents(context.Background(), agents, opts)
	if calls != 6 {
		t.Fatalf("changed agent not re-fetched: calls = %d", calls)
	}
}

func TestRateLimitRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`<html>` + strings.Repeat("x", 5000) + `</html>`))
	}))
	defer srv.Close()

	client := cursorapi.NewClient("test-key", srv.Client())
	client.SetBaseURL(srv.URL)
	_, err := client.Me(context.Background())
	apiErr, ok := err.(*cursorapi.APIError)
	if !ok || !apiErr.IsRateLimited() || apiErr.RetryAfter != 300 {
		t.Fatalf("err = %#v", err)
	}
	if strings.Contains(apiErr.Error(), "<html>") {
		t.Fatalf("raw body leaked into error: %q", apiErr.Error())
	}
}

func TestListAllAgentsStopsOnRepeatedCursor(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(cursorapi.AgentListResponse{
			Items:      []cursorapi.AgentListItem{{ID: "bc-1"}},
			NextCursor: "same",
		})
	}))
	defer srv.Close()

	client := cursorapi.NewClient("test-key", srv.Client())
	client.SetBaseURL(srv.URL)
	if _, err := client.ListAllAgents(context.Background(), cursorapi.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}
