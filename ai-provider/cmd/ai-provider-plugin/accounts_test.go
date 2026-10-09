package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubProvider returns a canned status per account so snapshot logic can be
// tested without network access.
type stubProvider struct {
	id, name string
	calls    atomic.Int32
	fetch    func(auth AuthContext) ProviderStatus
}

func (s *stubProvider) ID() string          { return s.id }
func (s *stubProvider) DisplayName() string { return s.name }
func (s *stubProvider) Fetch(_ context.Context, auth AuthContext) (ProviderStatus, error) {
	s.calls.Add(1)
	return s.fetch(auth), nil
}

func readyStatus(id, name string, pct float64) ProviderStatus {
	return ProviderStatus{
		ProviderID: id, DisplayName: name, Health: "ready", UsagePercent: pct,
		SummaryValue: fmt.Sprintf("%.0f%%", pct), WindowLabel: "Usage",
		Windows: []usageWindow{newWindow("usage", "Usage", pct, time.Time{})},
	}
}

func newTestPlugin(enabled string, providers []Provider, auths map[string][]AuthContext) *aiProviderPlugin {
	return &aiProviderPlugin{
		config:       parseConfig(map[string]string{"enabledProviders": enabled}),
		providerAuth: auths,
		prevUsage:    map[string]float64{},
		providers:    providers,
	}
}

func claudeUsageHandler(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, claudeSampleUsage) }

func TestDedupeClaudeRecordsWithoutTokensShareLocalLogin(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("local", env.now.Add(time.Hour)))
	env.handler = claudeUsageHandler
	p := newTestPlugin("claude", []Provider{env.provider}, map[string][]AuthContext{
		"claude": {{ProviderID: "claude", AccountID: "a"}, {ProviderID: "claude", AccountID: "b", DisplayName: "Claude"}},
	})
	snap := p.buildSnapshot()
	if len(snap.Items) != 1 || env.hits.Load() != 1 {
		t.Fatalf("expected one card from one request, got %d items, %d requests", len(snap.Items), env.hits.Load())
	}
	if snap.Items[0].ID != "ai-provider-claude-a" || snap.Items[0].Title != "Claude" {
		t.Fatalf("unexpected item: %+v", snap.Items[0])
	}
}

func TestDedupeClaudeIdenticalTokens(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.handler = claudeUsageHandler
	p := newTestPlugin("claude", []Provider{env.provider}, map[string][]AuthContext{
		"claude": {{AccountID: "a", APIKey: "tok"}, {AccountID: "b", AccessToken: " tok "}},
	})
	snap := p.buildSnapshot()
	if len(snap.Items) != 1 || env.hits.Load() != 1 {
		t.Fatalf("expected one card, got %d items, %d requests", len(snap.Items), env.hits.Load())
	}
}

func TestDedupeClaudeDistinctTokensGiveTwoCards(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.handler = claudeUsageHandler
	p := newTestPlugin("claude", []Provider{env.provider}, map[string][]AuthContext{
		"claude": {{AccountID: "a", APIKey: "tok-1"}, {AccountID: "b", APIKey: "tok-2"}},
	})
	snap := p.buildSnapshot()
	if len(snap.Items) != 2 {
		t.Fatalf("expected two cards, got %+v", snap.Items)
	}
	if snap.Items[0].ID == snap.Items[1].ID || snap.Items[0].Subtitle == snap.Items[1].Subtitle {
		t.Fatalf("cards not distinguishable: %+v", snap.Items)
	}
	if snap.Items[0].Subtitle != "Account 1" || snap.Items[1].Subtitle != "Account 2" {
		t.Fatalf("subtitles = %q, %q", snap.Items[0].Subtitle, snap.Items[1].Subtitle)
	}
}

func TestDedupeClaudePastedTokenEqualToLocalLogin(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("same", env.now.Add(time.Hour)))
	env.handler = claudeUsageHandler
	p := newTestPlugin("claude", []Provider{env.provider}, map[string][]AuthContext{
		"claude": {{AccountID: "a"}, {AccountID: "b", APIKey: "same"}},
	})
	for i := 0; i < 2; i++ {
		env.now = env.now.Add(2 * claudeMinFetchInterval)
		if snap := p.buildSnapshot(); len(snap.Items) != 1 {
			t.Fatalf("refresh %d: expected one card, got %+v", i, snap.Items)
		}
	}
}

func TestClaudeProfileLabelsAndCollapsesSameAccount(t *testing.T) {
	env := newClaudeTestEnv(t)
	claudeProfileURL = env.serverURL + "/api/oauth/profile"
	var profileHits atomic.Int32
	env.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/profile" {
			profileHits.Add(1)
			if r.Header.Get("anthropic-beta") != claudeOAuthBetaHeader {
				t.Error("profile request missing beta header")
			}
			fmt.Fprint(w, `{"account":{"uuid":"u-1","email":"dev@example.com","full_name":"Dev"},"organization":{"uuid":"o-1","name":"Org"}}`)
			return
		}
		fmt.Fprint(w, claudeSampleUsage)
	}
	p := newTestPlugin("claude", []Provider{env.provider}, map[string][]AuthContext{
		"claude": {{AccountID: "a", APIKey: "tok-1"}, {AccountID: "b", APIKey: "tok-2"}},
	})
	snap := p.buildSnapshot()
	if len(snap.Items) != 1 || snap.Items[0].Subtitle != "dev@example.com" {
		t.Fatalf("expected one labelled card, got %+v", snap.Items)
	}
	env.now = env.now.Add(2 * claudeMinFetchInterval)
	p.buildSnapshot()
	if profileHits.Load() != 2 {
		t.Fatalf("profile should be fetched once per token, got %d", profileHits.Load())
	}
}

func TestEmptySecretRecordsDoNotAddAuthCards(t *testing.T) {
	stub := &stubProvider{id: "opencode-go", name: "OpenCode Go", fetch: func(a AuthContext) ProviderStatus {
		return readyStatus("opencode-go", "OpenCode Go", 10)
	}}
	p := newTestPlugin("opencode-go", []Provider{stub}, map[string][]AuthContext{
		"opencode-go": {{AccountID: "x"}, {AccountID: "y", CookieHeader: "s=1"}, {AccountID: "z"}},
	})
	snap := p.buildSnapshot()
	if len(snap.Items) != 1 || snap.Items[0].ID != "ai-provider-opencode-go-y" {
		t.Fatalf("expected only the working card, got %+v", snap.Items)
	}

	p = newTestPlugin("opencode-go", []Provider{stub}, map[string][]AuthContext{
		"opencode-go": {{AccountID: "x"}, {AccountID: "z"}},
	})
	snap = p.buildSnapshot()
	if len(snap.Items) != 1 || snap.Items[0].Subtitle != "Auth required" {
		t.Fatalf("expected one auth_required card, got %+v", snap.Items)
	}
}

func TestOpenCodeGoFallbackDedupesIdenticalCookies(t *testing.T) {
	stub := &stubProvider{id: "opencode-go", name: "OpenCode Go", fetch: func(a AuthContext) ProviderStatus {
		return readyStatus("opencode-go", "OpenCode Go", 10)
	}}
	p := newTestPlugin("opencode-go", []Provider{stub}, map[string][]AuthContext{
		"opencode": {{AccountID: "x", CookieHeader: "s=1;  t=2"}, {AccountID: "y", CookieHeader: "s=1; t=2"}},
	})
	if snap := p.buildSnapshot(); len(snap.Items) != 1 || stub.calls.Load() != 1 {
		t.Fatalf("expected one card, got %+v", snap.Items)
	}
}

func TestNamingSameHostLabelGetsDistinctSubtitles(t *testing.T) {
	stub := &stubProvider{id: "opencode-go", name: "OpenCode Go", fetch: func(a AuthContext) ProviderStatus {
		return readyStatus("opencode-go", "OpenCode Go", 10)
	}}
	p := newTestPlugin("opencode-go", []Provider{stub}, map[string][]AuthContext{
		"opencode-go": {
			{AccountID: "acct-b", CookieHeader: "b", DisplayName: "OpenCode Go"},
			{AccountID: "acct-a", CookieHeader: "a", DisplayName: "OpenCode Go"},
		},
	})
	snap := p.buildSnapshot()
	if len(snap.Items) != 2 {
		t.Fatalf("expected two cards, got %+v", snap.Items)
	}
	// Numbered by account id order, title is the provider name.
	if snap.Items[0].ID != "ai-provider-opencode-go-acct-a" || snap.Items[0].Subtitle != "Account 1" || snap.Items[1].Subtitle != "Account 2" {
		t.Fatalf("unexpected items: %+v", snap.Items)
	}
	if snap.Items[0].Title != "OpenCode Go" {
		t.Fatalf("title = %q", snap.Items[0].Title)
	}

	p.providerAuth["opencode-go"][0].DisplayName = "Work"
	p.providerAuth["opencode-go"][1].DisplayName = "Work"
	snap = p.buildSnapshot()
	if snap.Items[0].Subtitle != "Work · acct-a" || snap.Items[1].Subtitle != "Work · acct-b" {
		t.Fatalf("ambiguous labels not disambiguated: %q, %q", snap.Items[0].Subtitle, snap.Items[1].Subtitle)
	}
}

func TestAssignAccountLabelsPrefersProviderIdentityAndStaysUnique(t *testing.T) {
	group := []ProviderStatus{
		{DisplayName: "Copilot", ProviderID: "copilot", AccountIdentity: "octocat", AccountDisplayName: "Work"},
		{DisplayName: "Copilot", ProviderID: "copilot", AccountDisplayName: "Work"},
		{DisplayName: "Copilot", ProviderID: "copilot", AccountDisplayName: "Work"},
	}
	assignAccountLabels(group)
	seen := map[string]bool{}
	for _, c := range group {
		if seen[c.AccountLabel] {
			t.Fatalf("duplicate label %q in %+v", c.AccountLabel, group)
		}
		seen[c.AccountLabel] = true
	}
	if group[0].AccountLabel != "octocat" {
		t.Fatalf("label = %q", group[0].AccountLabel)
	}

	single := []ProviderStatus{{DisplayName: "Claude", ProviderID: "claude", AccountDisplayName: "claude"}}
	assignAccountLabels(single)
	if single[0].AccountLabel != "" {
		t.Fatalf("provider-name host label should be ignored, got %q", single[0].AccountLabel)
	}
}

func TestAlertIDsIncludeAccount(t *testing.T) {
	stub := &stubProvider{id: "opencode-go", name: "OpenCode Go", fetch: func(a AuthContext) ProviderStatus {
		return readyStatus("opencode-go", "OpenCode Go", 95)
	}}
	p := newTestPlugin("opencode-go", []Provider{stub}, map[string][]AuthContext{
		"opencode-go": {{AccountID: "a", CookieHeader: "a"}, {AccountID: "b", CookieHeader: "b"}},
	})
	snap := p.buildSnapshot()
	ids := map[string]bool{}
	for _, a := range snap.Alerts {
		if ids[a.ID] {
			t.Fatalf("duplicate alert id %q", a.ID)
		}
		ids[a.ID] = true
	}
	if !ids["ai-opencode-go-a-critical"] || !ids["ai-opencode-go-b-critical"] {
		t.Fatalf("unexpected alerts: %+v", snap.Alerts)
	}
}

func TestClaudeGauges(t *testing.T) {
	status, err := parseClaudeUsage([]byte(claudeSampleUsage), "max", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status.Plan != "Max" {
		t.Fatalf("plan = %q", status.Plan)
	}
	item := cardToItem(status)
	if len(item.Gauges) != 3 {
		t.Fatalf("expected session/weekly/sonnet gauges, got %+v", item.Gauges)
	}
	want := []struct {
		label string
		value float64
	}{{"Session", 0.33}, {"Weekly", 0.41}, {"Weekly · Sonnet", 0.01}}
	for i, w := range want {
		g := item.Gauges[i]
		if g.Label != w.label || g.Value != w.value {
			t.Fatalf("gauge %d = %+v, want %s %.2f", i, g, w.label, w.value)
		}
	}
	if !strings.HasPrefix(item.Gauges[0].Caption, "resets in ") || item.Gauges[2].Caption != "" {
		t.Fatalf("captions = %q / %q", item.Gauges[0].Caption, item.Gauges[2].Caption)
	}
	// The percentages live in the gauges; the detail must not repeat them.
	if strings.Contains(item.Detail, "%") {
		t.Fatalf("detail duplicates gauge text: %q", item.Detail)
	}
	if item.Subtitle != "Max" {
		t.Fatalf("subtitle = %q", item.Subtitle)
	}
}

func TestGaugeSeverityUsesConfiguredThresholds(t *testing.T) {
	env := newClaudeTestEnv(t)
	env.writeCreds(t, claudeCredsJSON("tok", env.now.Add(time.Hour)))
	env.handler = claudeUsageHandler
	p := &aiProviderPlugin{
		config:       parseConfig(map[string]string{"enabledProviders": "claude", "warningThreshold": "30", "criticalThreshold": "40"}),
		providerAuth: map[string][]AuthContext{},
		prevUsage:    map[string]float64{},
		providers:    []Provider{env.provider},
	}
	gauges := p.buildSnapshot().Items[0].Gauges
	if gauges[0].Severity != "warning" || gauges[1].Severity != "critical" || gauges[2].Severity != "info" {
		t.Fatalf("unexpected gauge severities: %+v", gauges)
	}
}

func TestCreditGauge(t *testing.T) {
	w, ok := amountWindow("credits", "Credits", 4.2, 10, formatUSD, time.Time{})
	if !ok {
		t.Fatal("expected a gauge")
	}
	g := w.gauge()
	if g.ValueLabel != "$4.20 / $10" || g.Value < 0.419 || g.Value > 0.421 {
		t.Fatalf("unexpected credit gauge: %+v", g)
	}
	if _, ok := amountWindow("credits", "Credits", 1, 0, formatUSD, time.Time{}); ok {
		t.Fatal("zero total must not produce a gauge")
	}

	item := cardToItem(ProviderStatus{
		ProviderID: "openrouter", DisplayName: "OpenRouter", Health: "ready",
		WindowLabel: "Credit balance", Details: "$5.80 remaining", Windows: []usageWindow{w},
	})
	if len(item.Gauges) != 1 || item.Subtitle != "Credit balance" || item.Detail != "" {
		t.Fatalf("unexpected item: %+v", item)
	}
}
