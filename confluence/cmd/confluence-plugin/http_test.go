package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lum1n/smuler/plugins/sdk-go"
)

func newTestHandler(srvURL string, auth confluenceAuth) *handler {
	return &handler{
		client:    &http.Client{Timeout: 5 * time.Second},
		domain:    normalizeDomain(srvURL),
		maxItems:  defaultMax,
		auth:      auth,
		prevPages: map[string]prevPageInfo{},
		peerKeys:  map[string]struct{}{},
	}
}

func TestGetStatusEmptySearchMakesSingleRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"size":0}`))
	}))
	defer srv.Close()

	snap := newTestHandler(srv.URL, confluenceAuth{Kind: "api_key", APIKey: "u:p"}).GetStatus()
	if snap.Health != sdk.HealthOK {
		t.Fatalf("health=%q", snap.Health)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected 1 request for empty feed, got %d", n)
	}
}

func TestGetStatusUnauthorizedNoFallback(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"secret-ish body detail"}`))
	}))
	defer srv.Close()

	snap := newTestHandler(srv.URL, confluenceAuth{Kind: "api_key", APIKey: "u:p"}).GetStatus()
	if snap.Health != sdk.HealthAuthReq {
		t.Fatalf("health=%q want auth_required", snap.Health)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected no fallback request on 401, got %d calls", n)
	}
}

func TestGetStatusRateLimitedUsesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	snap := newTestHandler(srv.URL, confluenceAuth{Kind: "api_key", APIKey: "u:p"}).GetStatus()
	if snap.Health != sdk.HealthRateLimited || snap.RefreshAfter != 1200 {
		t.Fatalf("health=%q refreshAfter=%d", snap.Health, snap.RefreshAfter)
	}
}

func TestCookieSessionLoginRedirectIsAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rest/api/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login.action?os_destination=x", http.StatusFound)
	})
	mux.HandleFunc("/login.action", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>login</html>"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	snap := newTestHandler(srv.URL, confluenceAuth{Kind: "browser_import", CookieHeader: "JSESSIONID=x"}).GetStatus()
	if snap.Health != sdk.HealthAuthReq {
		t.Fatalf("health=%q want auth_required", snap.Health)
	}
	for _, a := range snap.Alerts {
		if strings.Contains(a.Message, "parse") || strings.Contains(a.Message, "<html") {
			t.Fatalf("alert leaks parse/body detail: %q", a.Message)
		}
	}
}

func TestGetStatusParsesSearchHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Errorf("missing auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"content":{"id":"123","type":"page","title":"Runbook ENG-7","space":{"key":"ENG"},
			 "version":{"number":3,"when":"2024-01-01T10:00:00.000+01:00","by":null},"_links":{"webui":"/display/ENG/Runbook"}},
			 "excerpt":"hello","url":"/display/ENG/Runbook"},
			{"title":"a space hit","url":"/spaces/ENG"}
		]}`))
	}))
	defer srv.Close()

	snap := newTestHandler(srv.URL, confluenceAuth{Kind: "api_key", APIKey: "u:p"}).GetStatus()
	if len(snap.Items) != 1 {
		t.Fatalf("items=%d want 1 (non-content hit skipped)", len(snap.Items))
	}
	it := snap.Items[0]
	if it.ID != "123" || it.Timestamp != "2024-01-01T09:00:00Z" || it.DeepLink != srv.URL+"/display/ENG/Runbook" {
		t.Fatalf("unexpected item: %+v", it)
	}
}

func TestAuthApplyHonorsKind(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	confluenceAuth{Kind: "oauth", APIKey: "u:p", AccessToken: "tok"}.apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization=%q want bearer", got)
	}
	req2, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	confluenceAuth{Kind: "browser_import", APIKey: "u:p", CookieHeader: "a=b"}.apply(req2)
	if req2.Header.Get("Authorization") != "" || req2.Header.Get("Cookie") != "a=b" {
		t.Fatalf("cookie kind not honored: %v", req2.Header)
	}
}

func TestAPIErrorOmitsRawBody(t *testing.T) {
	resp := &http.Response{StatusCode: 500, Header: http.Header{}}
	e := newAPIHTTPError(resp, []byte("<html>stack trace token=abc</html>"))
	if strings.Contains(e.Error(), "token") {
		t.Fatalf("error leaks body: %q", e.Error())
	}
	e = newAPIHTTPError(&http.Response{StatusCode: 400, Header: http.Header{}}, []byte(`{"message":"Could not parse cql"}`))
	if e.Error() != "HTTP 400: Could not parse cql" {
		t.Fatalf("got %q", e.Error())
	}
}
