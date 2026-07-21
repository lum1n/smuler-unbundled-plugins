package main

import (
	"encoding/json"
	"testing"
)

func TestOpencodeCookieHeaderKeepsSessionCookiesAndDropsAttributes(t *testing.T) {
	input := "session=abc123; auth=token; Path=/; Secure; HttpOnly; SameSite=Lax; theme=dark; Domain=opencode.ai"
	want := "session=abc123; auth=token; theme=dark"

	got := opencodeCookieHeader(input)
	if got != want {
		t.Fatalf("opencodeCookieHeader() = %q, want %q", got, want)
	}
}

func TestOpencodePercentDoesNotScaleAlreadyPercentageValues(t *testing.T) {
	tests := []struct {
		raw  float64
		want float64
	}{
		{0, 0},
		{0.5, 0.5},
		{1, 1},
		{1.0, 1},
		{19.5, 19.5},
		{100, 100},
		{-5, 0},
		{150, 100},
	}
	for _, tc := range tests {
		if got := opencodePercent(tc.raw); got != tc.want {
			t.Fatalf("opencodePercent(%v) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestOpencodeExtractPercentKeepsOnePercent(t *testing.T) {
	window := map[string]interface{}{
		"usagePercent": 1.0,
		"resetInSec":   3600,
	}
	if got := opencodeExtractPercent(window); got != 1 {
		t.Fatalf("opencodeExtractPercent() = %v, want 1", got)
	}

	data := map[string]interface{}{
		"rollingUsage": map[string]interface{}{
			"usagePercent": 1.0,
			"resetInSec":   4907,
		},
		"weeklyUsage": map[string]interface{}{
			"usagePercent": 1.0,
			"resetInSec":   126983,
		},
	}
	rollingPct, _ := opencodeExtractWindow(data, "rolling")
	weeklyPct, _ := opencodeExtractWindow(data, "weekly")
	// Fetch applies opencodePercent again after extract; ensure double clamp stays at 1%.
	rollingPct = opencodePercent(rollingPct)
	weeklyPct = opencodePercent(weeklyPct)
	if rollingPct != 1 || weeklyPct != 1 {
		t.Fatalf("rolling=%v weekly=%v, want 1 and 1", rollingPct, weeklyPct)
	}
}

func TestCopilotAPIHost(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"", "api.github.com"},
		{"github.com", "api.github.com"},
		{"https://octocorp.ghe.com", "api.octocorp.ghe.com"},
		{"api.octocorp.ghe.com", "api.octocorp.ghe.com"},
	}
	for _, tc := range tests {
		if got := copilotAPIHost(tc.host); got != tc.want {
			t.Fatalf("copilotAPIHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestCopilotQuotaSnapshotUsedPercent(t *testing.T) {
	snapshot := &copilotQuotaSnapshot{PercentRemaining: 25}
	if got := snapshot.usedPercent(); got != 75 {
		t.Fatalf("usedPercent() = %v, want 75", got)
	}
}

func TestCopilotQuotaSnapshotPlaceholder(t *testing.T) {
	placeholder := &copilotQuotaSnapshot{}
	if !placeholder.isPlaceholder() {
		t.Fatal("expected zero snapshot to be placeholder")
	}
	usable := &copilotQuotaSnapshot{Entitlement: 100, Remaining: 40, PercentRemaining: 40}
	if usable.isPlaceholder() {
		t.Fatal("expected usable snapshot not to be placeholder")
	}
}

func TestParseCopilotUsageResponse(t *testing.T) {
	raw := `{
		"copilot_plan": "pro",
		"quota_reset_date": "2026-08-01",
		"quota_snapshots": {
			"premium_interactions": {
				"entitlement": 100,
				"remaining": 20,
				"percent_remaining": 20,
				"quota_id": "premium"
			},
			"chat": {
				"entitlement": 100,
				"remaining": 50,
				"percent_remaining": 50,
				"quota_id": "chat"
			}
		}
	}`

	var usage copilotUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("unmarshal copilot usage: %v", err)
	}

	premium := usage.QuotaSnapshots.PremiumInteractions
	if premium == nil || premium.usedPercent() != 80 {
		t.Fatalf("premium usedPercent = %v, want 80", premium)
	}
	chat := usage.QuotaSnapshots.Chat
	if chat == nil || chat.usedPercent() != 50 {
		t.Fatalf("chat usedPercent = %v, want 50", chat)
	}
}

func TestCursorPlanPercentFromSummary(t *testing.T) {
	total := 42.5
	auto := 30.0
	api := 55.0
	summary := cursorUsageSummary{
		MembershipType: strPtr("pro"),
		IndividualUsage: &cursorIndividualUsage{
			Plan: &cursorPlanUsage{
				TotalPercentUsed: &total,
				AutoPercentUsed:  &auto,
				APIPercentUsed:   &api,
			},
		},
	}

	if got := cursorPlanPercent(summary, nil); got != 42.5 {
		t.Fatalf("cursorPlanPercent() = %v, want 42.5", got)
	}
}

func TestCursorPlanPercentFromLegacyRequests(t *testing.T) {
	used := 120
	limit := 500
	summary := cursorUsageSummary{}
	requestUsage := &cursorLegacyUsage{}
	requestUsage.GPT4.NumRequestsTotal = &used
	requestUsage.GPT4.MaxRequestUsage = &limit

	if got := cursorPlanPercent(summary, requestUsage); got != 24 {
		t.Fatalf("cursorPlanPercent() = %v, want 24", got)
	}
}

func TestCursorNormalizePercent(t *testing.T) {
	v := 105.0
	if got := cursorNormalizePercent(&v); got != 100 {
		t.Fatalf("cursorNormalizePercent() = %v, want 100", got)
	}
}

func TestCommandCodeSessionCookie(t *testing.T) {
	raw := "_ga=GA1.2.123; __Secure-better-auth.session_token=abc123; foo=bar"
	got, ok := commandCodeSessionCookie(raw)
	if !ok || got != "__Secure-better-auth.session_token=abc123" {
		t.Fatalf("commandCodeSessionCookie() = %q, %v", got, ok)
	}

	bare, ok := commandCodeSessionCookie("bare-token")
	if !ok || bare != "__Secure-better-auth.session_token=bare-token" {
		t.Fatalf("commandCodeSessionCookie(bare) = %q, %v", bare, ok)
	}
}

func TestCommandCodeParseCredits(t *testing.T) {
	raw := `{"credits":{"monthlyCredits":8.7784,"purchasedCredits":0,"premiumMonthlyCredits":0,"opensourceMonthlyCredits":8.7784}}`
	payload, err := commandCodeParseCredits([]byte(raw))
	if err != nil {
		t.Fatalf("commandCodeParseCredits() error: %v", err)
	}
	if payload.MonthlyCredits != 8.7784 || payload.OpensourceMonthlyCredits != 8.7784 {
		t.Fatalf("unexpected credits payload: %+v", payload)
	}
}

func TestCommandCodeParseSubscription(t *testing.T) {
	raw := `{"success":true,"data":{"planId":"individual-go","status":"active","currentPeriodEnd":"2026-06-06T07:28:50.000Z"}}`
	payload, err := commandCodeParseSubscription([]byte(raw))
	if err != nil || payload == nil {
		t.Fatalf("commandCodeParseSubscription() = %+v, %v", payload, err)
	}
	if payload.PlanID != "individual-go" || payload.Status != "active" {
		t.Fatalf("unexpected subscription payload: %+v", payload)
	}

	freeTier, err := commandCodeParseSubscription([]byte(`{"success":true,"data":null}`))
	if err != nil || freeTier != nil {
		t.Fatalf("free tier subscription = %+v, %v", freeTier, err)
	}
}

func TestCommandCodeUsagePercent(t *testing.T) {
	if got := commandCodeUsagePercent(8.7784, 10); got < 12.2 || got > 12.3 {
		t.Fatalf("commandCodeUsagePercent() = %v, want ~12.216", got)
	}
}

func strPtr(v string) *string {
	return &v
}
