package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const claudeSampleUsage = `{
	"five_hour": {"utilization": 33.0, "resets_at": "2099-04-11T07:00:00.528743+00:00"},
	"seven_day": {"utilization": 41.0, "resets_at": "2099-04-17T00:59:59.951713+00:00"},
	"seven_day_opus": null,
	"seven_day_sonnet": {"utilization": 1.0, "resets_at": null},
	"extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null}
}`

func claudeCredsJSON(token string, expiresAt time.Time, scopes ...string) string {
	if len(scopes) == 0 {
		scopes = []string{"user:inference", "user:profile"}
	}
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"rt","expiresAt":%d,"scopes":["%s"],"subscriptionType":"max"}}`,
		token, expiresAt.UnixMilli(), strings.Join(scopes, `","`))
}

type claudeTestEnv struct {
	home     string
	hits     atomic.Int32
	handler  func(w http.ResponseWriter, r *http.Request)
	provider *claudeProvider
	now      time.Time
}

func newClaudeTestEnv(t *testing.T) *claudeTestEnv {
	t.Helper()
	env := &claudeTestEnv{home: t.TempDir(), now: time.Now()}
	env.provider = &claudeProvider{now: func() time.Time { return env.now }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.hits.Add(1)
		env.handler(w, r)
	}))
	t.Cleanup(srv.Close)

	oldURL, oldHome, oldKeychain := claudeUsageURL, claudeHomeDir, claudeKeychainReader
	claudeUsageURL = srv.URL + "/api/oauth/usage"
	claudeHomeDir = env.home
	claudeKeychainReader = func(context.Context) ([]byte, error) { return nil, errClaudeNoCredentials }
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Cleanup(func() { claudeUsageURL, claudeHomeDir, claudeKeychainReader = oldURL, oldHome, oldKeychain })
	return env
}

func (e *claudeTestEnv) writeCreds(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(e.home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseClaudeCredentialsJSON(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	raw := claudeCredsJSON("tok", exp)
	c, err := parseClaudeCredentialsJSON([]byte(raw), "file")
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "tok" || !c.ExpiresAt.Equal(exp) || c.SubscriptionType != "max" || c.missingProfileScope() {
		t.Fatalf("unexpected creds: %+v", c)
	}

	hexed, err := parseClaudeCredentialsJSON([]byte(hex.EncodeToString([]byte(raw))+"\n"), "keychain")
	if err != nil || hexed.AccessToken != "tok" {
		t.Fatalf("hex-encoded keychain blob not parsed: %+v %v", hexed, err)
	}

	if _, err := parseClaudeCredentialsJSON([]byte(`{"mcpOAuth":{}}`), "keychain"); err != errClaudeNoCredentials {
		t.Fatalf("expected errClaudeNoCredentials, got %v", err)
	}

	inferenceOnly, _ := parseClaudeCredentialsJSON([]byte(claudeCredsJSON("tok", exp, "user:inference")), "file")
	if !inferenceOnly.missingProfileScope() {
		t.Fatal("expected missing user:profile scope")
	}
}

func TestParseClaudeUsagePicksHighestWindow(t *testing.T) {
	status, err := parseClaudeUsage([]byte(claudeSampleUsage), "max", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.UsagePercent != 41 || status.Health != "ready" {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.WindowLabel != "Max · Weekly usage" {
		t.Fatalf("window label = %q", status.WindowLabel)
	}
	if !strings.Contains(status.Details, "Session 33%") || !strings.Contains(status.Details, "Sonnet weekly 1%") || strings.Contains(status.Details, "Opus") {
		t.Fatalf("details = %q", status.Details)
	}
	if status.ResetAt.IsZero() {
		t.Fatal("expected reset time from fractional-second RFC3339 timestamp")
	}

	if _, err := parseClaudeUsage([]byte(`{"five_hour":null}`), "", time.Now()); err == nil {
		t.Fatal("expected error when no windows present")
	}
}

func TestClaudeFetchUsesLocalCredentialsAndHeaders(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("local-token", env.now.Add(time.Hour)))
	env.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer local-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta = %q", got)
		}
		if r.Header.Get("x-api-key") != "" {
			t.Error("x-api-key must not be sent")
		}
		fmt.Fprint(w, claudeSampleUsage)
	}

	status, err := env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Health != "ready" || status.UsagePercent != 41 {
		t.Fatalf("unexpected status: %+v", status)
	}

	// Second call inside the min interval is served from cache.
	env.now = env.now.Add(10 * time.Second)
	if _, err := env.provider.Fetch(context.Background(), AuthContext{}); err != nil {
		t.Fatal(err)
	}
	if env.hits.Load() != 1 {
		t.Fatalf("expected 1 request, got %d", env.hits.Load())
	}

	env.now = env.now.Add(claudeMinFetchInterval)
	if _, err := env.provider.Fetch(context.Background(), AuthContext{}); err != nil {
		t.Fatal(err)
	}
	if env.hits.Load() != 2 {
		t.Fatalf("expected refetch after interval, got %d requests", env.hits.Load())
	}
}

func TestClaudeFetchSettingsTokenTakesPrecedence(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("local-token", env.now.Add(time.Hour)))
	env.handler = func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer pasted" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, claudeSampleUsage)
	}
	status, err := env.provider.Fetch(context.Background(), AuthContext{APIKey: "pasted"})
	if err != nil || status.Health != "ready" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestClaudeFetchAPIKeyWithoutLocalLoginIsAuthRequired(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.handler = func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") }
	status, err := env.provider.Fetch(context.Background(), AuthContext{APIKey: "sk-ant-api03-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Health != "auth_required" || !strings.Contains(status.Details, "API keys") {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestClaudeFetchNoCredentials(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.handler = func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request") }
	status, err := env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil || status.Health != "auth_required" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestClaudeFetchExpiredLocalTokenIsAuthRequired(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("old", env.now.Add(-time.Minute)))
	env.handler = func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected request with expired token") }
	status, err := env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil || status.Health != "auth_required" || !strings.Contains(status.Details, "expired") {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestClaudeFetchRereadsCredentialsOn401(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("stale", env.now.Add(time.Hour)))
	env.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer stale" {
			// Simulate Claude Code rotating the token meanwhile.
			env.writeCreds(t, claudeCredsJSON("fresh", env.now.Add(time.Hour)))
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, claudeSampleUsage)
	}
	status, err := env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil || status.Health != "ready" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if env.hits.Load() != 2 {
		t.Fatalf("expected one retry, got %d requests", env.hits.Load())
	}
}

func TestClaudeFetchRateLimitBackoff(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("tok", env.now.Add(24*time.Hour)))
	limited := false
	env.handler = func(w http.ResponseWriter, r *http.Request) {
		if limited {
			w.Header().Set("Retry-After", "300")
			http.Error(w, `{"error":{"type":"rate_limit_error"}}`, http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, claudeSampleUsage)
	}

	if _, err := env.provider.Fetch(context.Background(), AuthContext{}); err != nil {
		t.Fatal(err)
	}
	limited = true
	env.now = env.now.Add(2 * claudeMinFetchInterval)
	status, err := env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Health != "ready" || status.UsagePercent != 41 || !strings.Contains(status.Details, "rate limited") {
		t.Fatalf("expected stale reading during backoff, got %+v", status)
	}
	hits := env.hits.Load()

	// Within Retry-After: no network.
	env.now = env.now.Add(4 * time.Minute)
	if _, err := env.provider.Fetch(context.Background(), AuthContext{}); err != nil {
		t.Fatal(err)
	}
	if env.hits.Load() != hits {
		t.Fatal("request sent during backoff window")
	}

	// After Retry-After: tries again and recovers.
	limited = false
	env.now = env.now.Add(2 * time.Minute)
	status, err = env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil || status.Health != "ready" || strings.Contains(status.Details, "rate limited") {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestClaudeFetchRateLimitedWithoutCache(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("tok", env.now.Add(time.Hour)))
	env.handler = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}
	status, err := env.provider.Fetch(context.Background(), AuthContext{})
	if err != nil || status.Health != "rate_limited" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	item := cardToItem(status)
	if item.Severity != "warning" || item.Subtitle != "Rate limited" {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("120", now); got != 2*time.Minute {
		t.Fatalf("got %v", got)
	}
	if got := parseRetryAfter(now.Add(time.Hour).Format(http.TimeFormat), now); got != time.Hour {
		t.Fatalf("got %v", got)
	}
	if got := parseRetryAfter("garbage", now); got != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestFormatHTTPErrorRateLimit(t *testing.T) {
	if !isRateLimitError(formatHTTPError("X", 429, nil)) {
		t.Fatal("429 should be a rate limit error")
	}
	if isRateLimitError(formatHTTPError("X", 500, []byte("boom"))) {
		t.Fatal("500 is not a rate limit error")
	}
}

func TestSeverityHelpers(t *testing.T) {
	p := &aiProviderPlugin{config: aiProviderConfig{WarningThreshold: 50, CriticalThreshold: 80}}
	if p.severityFor(60) != "warning" || p.severityFor(80) != "critical" || p.severityFor(10) != "info" {
		t.Fatal("severityFor ignores configured thresholds")
	}
	if validSeverity("ready") != "info" || validSeverity("critical") != "critical" {
		t.Fatal("validSeverity")
	}
}

func TestBuildSnapshotClaudeLocalAuthWithoutHostCredential(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("tok", env.now.Add(time.Hour)))
	env.handler = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, claudeSampleUsage) }

	p := &aiProviderPlugin{
		config:       parseConfig(map[string]string{"enabledProviders": "claude", "warningThreshold": "40"}),
		providerAuth: map[string][]AuthContext{},
		prevUsage:    map[string]float64{},
		providers:    []Provider{env.provider},
	}
	snap := p.buildSnapshot()
	if snap.Health != "ok" || len(snap.Items) != 1 {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap.Items[0].Severity != "warning" || snap.Summary.Value != "41%" {
		t.Fatalf("unexpected item/summary: %+v / %+v", snap.Items[0], snap.Summary)
	}
}

func TestGeminiExpiryDateMillis(t *testing.T) {
	creds, err := geminiParseOAuthCreds([]byte(`{"access_token":"a","refresh_token":"r","expiry_date":1760000000000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !creds.ExpiryDate.Equal(time.UnixMilli(1760000000000).UTC()) {
		t.Fatalf("expiry = %v", creds.ExpiryDate)
	}
}
