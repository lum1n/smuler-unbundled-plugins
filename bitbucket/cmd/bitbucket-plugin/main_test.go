package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

func newTestPlugin(srv *httptest.Server) *bitbucketPlugin {
	return &bitbucketPlugin{
		client:       srv.Client(),
		config:       bitbucketConfig{ShowMyPRs: true, ShowReviewRequests: true, MaxPRs: 10},
		auth:         bitbucketAuth{Kind: "api_key", APIKey: "me:secret"},
		apiBase:      srv.URL + "/2.0",
		webBase:      srv.URL,
		workspace:    "ws",
		prevPRStates: make(map[string]prStateInfo),
		prCache:      make(map[string]bbPR),
	}
}

func TestCookieAuthUsesWebAPIProxy(t *testing.T) {
	p := &bitbucketPlugin{}
	p.Initialize(sdk.InitializeParams{
		Config:        map[string]string{"workspace": "ws"},
		ProviderAuths: []sdk.ProviderAuthContext{{Kind: "browser_import", CookieHeader: "session=abc"}},
	})
	if p.apiBase != cloudCookieAPIBase {
		t.Fatalf("apiBase = %q, want %q", p.apiBase, cloudCookieAPIBase)
	}
	p.Initialize(sdk.InitializeParams{
		Config:        map[string]string{"workspace": "ws"},
		ProviderAuths: []sdk.ProviderAuthContext{{Kind: "oauth", AccessToken: "tok"}},
	})
	if p.apiBase != cloudAPIBase || p.auth.CookieHeader != "" {
		t.Fatalf("re-initialize kept stale auth/base: base=%q", p.apiBase)
	}
}

func TestLoginRedirectIsAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/user", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login?next=/2.0/user", http.StatusFound)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body>Log in</body></html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap := newTestPlugin(srv).GetStatus()
	if snap.Health != sdk.HealthAuthReq {
		t.Fatalf("health = %q, want auth_required", snap.Health)
	}
}

func TestAllReposFailingReportsAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/user", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"uuid":"{me}","username":"me","display_name":"Me"}`)
	})
	mux.HandleFunc("/2.0/repositories/ws", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"values":[{"slug":"a","full_name":"ws/a"},{"slug":"b","full_name":"ws/b"}]}`)
	})
	mux.HandleFunc("/2.0/repositories/ws/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, strings.Repeat("x", 5000))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap := newTestPlugin(srv).GetStatus()
	if snap.Health != sdk.HealthAuthReq {
		t.Fatalf("health = %q, want auth_required", snap.Health)
	}
}

func TestRateLimitedPropagatesRetryAfter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1200")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap := newTestPlugin(srv).GetStatus()
	if snap.Health != sdk.HealthRateLimited || snap.RefreshAfter != 1200 {
		t.Fatalf("health=%q refreshAfter=%d", snap.Health, snap.RefreshAfter)
	}
}

func TestErrorBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, strings.Repeat("secret ", 1000))
	}))
	defer srv.Close()

	snap := newTestPlugin(srv).GetStatus()
	if len(snap.Alerts) != 1 || len(snap.Alerts[0].Message) > 300 {
		t.Fatalf("alert message not truncated: %d bytes", len(snap.Alerts[0].Message))
	}
}

func TestSamePRNumberAcrossReposAndCaching(t *testing.T) {
	var userCalls, repoCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/2.0/user", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&userCalls, 1)
		fmt.Fprint(w, `{"uuid":"{me}","username":"me","display_name":"Me"}`)
	})
	mux.HandleFunc("/2.0/repositories/ws", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&repoCalls, 1)
		fmt.Fprint(w, `{"values":[{"slug":"a","full_name":"ws/a"},{"slug":"b","full_name":"ws/b"}]}`)
	})
	prJSON := func(repo string) string {
		return fmt.Sprintf(`{"values":[{"id":1,"title":"PR in %s","state":"OPEN","author":{"uuid":"{me}","username":"me","display_name":"Me"},
			"updated_on":"2024-05-01T10:00:00.123456+00:00","source":{"branch":{"name":"feat"}},"destination":{"branch":{"name":"main"}},
			"participants":null,"reviewers":null}]}`, repo)
	}
	mux.HandleFunc("/2.0/repositories/ws/a/pullrequests", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, prJSON("a")) })
	mux.HandleFunc("/2.0/repositories/ws/b/pullrequests", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, prJSON("b")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestPlugin(srv)
	snap := p.GetStatus()
	if snap.Health != sdk.HealthOK || len(snap.Items) != 2 {
		t.Fatalf("health=%q items=%d, want ok/2", snap.Health, len(snap.Items))
	}
	if snap.Items[0].ID == snap.Items[1].ID {
		t.Fatalf("duplicate item ids %q", snap.Items[0].ID)
	}
	for _, it := range snap.Items {
		if it.Timestamp != "2024-05-01T10:00:00Z" {
			t.Fatalf("timestamp = %q", it.Timestamp)
		}
		if !strings.HasPrefix(it.Subtitle, "a ·") && !strings.HasPrefix(it.Subtitle, "b ·") {
			t.Fatalf("subtitle should show repo, got %q", it.Subtitle)
		}
	}
	if _, ok := p.lookupCachedPR("b#1"); !ok {
		t.Fatalf("lookupCachedPR(b#1) failed")
	}
	if _, ok := p.lookupCachedPR("1"); ok {
		t.Fatalf("ambiguous bare id should not resolve")
	}

	p.GetStatus()
	if userCalls != 1 || repoCalls != 1 {
		t.Fatalf("expected cached user/repos, got user=%d repos=%d", userCalls, repoCalls)
	}
}

func TestServerReviewStatusNormalized(t *testing.T) {
	p := &bitbucketPlugin{webBase: "https://bb.example.com", workspace: "PRJ"}
	pr := p.mapServerPR(serverPR{ID: 3, Participants: []serverParticipant{{Status: "NEEDS_WORK"}}}, bbRepo{Slug: "r"})
	if countChangesRequested(pr) != 1 {
		t.Fatalf("NEEDS_WORK not counted as changes requested")
	}
}
