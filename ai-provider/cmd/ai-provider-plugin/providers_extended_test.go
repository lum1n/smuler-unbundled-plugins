package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestProviderRequiresHostAuth(t *testing.T) {
	if providerRequiresHostAuth("gemini") || providerRequiresHostAuth("kiro") {
		t.Fatal("gemini and kiro should not require host auth")
	}
	if !providerRequiresHostAuth("cursor") {
		t.Fatal("cursor should require host auth")
	}
}

func TestWindsurfParseSessionJSON(t *testing.T) {
	raw := `{
		"devin_session_token": "sess",
		"devin_auth1_token": "auth1",
		"devin_account_id": "acct",
		"devin_primary_org_id": "org"
	}`
	auth, err := windsurfParseSession(raw)
	if err != nil {
		t.Fatalf("windsurfParseSession() error: %v", err)
	}
	if auth.SessionToken != "sess" || auth.Auth1Token != "auth1" || auth.AccountID != "acct" || auth.PrimaryOrgID != "org" {
		t.Fatalf("unexpected session auth: %+v", auth)
	}
}

func TestWindsurfDecodePlanStatus(t *testing.T) {
	planInfo := appendProtoString(nil, 2, "Pro")
	planField := appendProtoFieldKey(nil, 1, protoWireLengthDelim)
	planField = appendProtoVarint(planField, uint64(len(planInfo)))
	planField = append(planField, planInfo...)

	statusField := appendProtoFieldKey(nil, 14, protoWireVarint)
	statusField = appendProtoVarint(statusField, 70)
	weeklyField := appendProtoFieldKey(nil, 15, protoWireVarint)
	weeklyField = appendProtoVarint(weeklyField, 40)

	payload := append(planField, statusField...)
	payload = append(payload, weeklyField...)

	wrapper := appendProtoFieldKey(nil, 1, protoWireLengthDelim)
	wrapper = appendProtoVarint(wrapper, uint64(len(payload)))
	wrapper = append(wrapper, payload...)

	status, err := windsurfDecodePlanStatusResponse(wrapper)
	if err != nil {
		t.Fatalf("windsurfDecodePlanStatusResponse() error: %v", err)
	}
	if status.PlanName != "Pro" {
		t.Fatalf("PlanName = %q, want Pro", status.PlanName)
	}
	if status.DailyQuotaRemainingPercent != 70 || status.WeeklyQuotaRemainingPercent != 40 {
		t.Fatalf("unexpected percents: daily=%d weekly=%d", status.DailyQuotaRemainingPercent, status.WeeklyQuotaRemainingPercent)
	}
}

func TestGrokParseBillingResponse(t *testing.T) {
	usageBits := math.Float32bits(42.5)
	var payload []byte
	payload = appendProtoFieldKey(payload, 1, protoWireFixed32)
	payload = append(payload,
		byte(usageBits),
		byte(usageBits>>8),
		byte(usageBits>>16),
		byte(usageBits>>24),
	)

	frame := make([]byte, 5+len(payload))
	frame[4] = byte(len(payload))
	copy(frame[5:], payload)

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	snapshot, err := grokParseBillingResponse(frame, now)
	if err != nil {
		t.Fatalf("grokParseBillingResponse() error: %v", err)
	}
	if snapshot.UsedPercent < 42.4 || snapshot.UsedPercent > 42.6 {
		t.Fatalf("UsedPercent = %v, want ~42.5", snapshot.UsedPercent)
	}
}

func TestGrokGRPCWebDataFrames(t *testing.T) {
	payload := []byte{0x0a, 0x01, 0x02}
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	frames := grokGRPCWebDataFrames(frame)
	if len(frames) != 1 || string(frames[0]) != string(payload) {
		t.Fatalf("unexpected frames: %+v", frames)
	}
}

func TestGeminiLowestQuotaPercent(t *testing.T) {
	raw := `{
		"buckets": [
			{"modelId": "flash", "remainingFraction": 0.25, "resetTime": "2026-08-01T00:00:00Z"},
			{"modelId": "pro", "remainingFraction": 0.60, "resetTime": "2026-08-02T00:00:00Z"}
		]
	}`
	usagePct, resetAt, err := geminiLowestQuotaPercent([]byte(raw))
	if err != nil {
		t.Fatalf("geminiLowestQuotaPercent() error: %v", err)
	}
	if usagePct != 75 {
		t.Fatalf("usagePct = %v, want 75", usagePct)
	}
	if resetAt.IsZero() {
		t.Fatal("expected resetAt to be set")
	}
}

func TestDevinNormalizeOrg(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"my-org", "org/my-org"},
		{"org/acme", "org/acme"},
		{"https://app.devin.ai/org/acme/billing", "org/acme"},
	}
	for _, tc := range tests {
		if got := devinNormalizeOrg(tc.raw); got != tc.want {
			t.Fatalf("devinNormalizeOrg(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestDevinParseUsage(t *testing.T) {
	raw := `{
		"planName": "Team",
		"windows": {
			"daily": {"usedPercent": 30},
			"weekly": {"usedPercent": 55}
		}
	}`
	daily, weekly, plan, err := devinParseUsage([]byte(raw))
	if err != nil {
		t.Fatalf("devinParseUsage() error: %v", err)
	}
	if daily != 30 || weekly != 55 || plan != "Team" {
		t.Fatalf("daily=%v weekly=%v plan=%q", daily, weekly, plan)
	}
}

func TestFactoryParseLimits(t *testing.T) {
	raw := `{
		"limits": {
			"standard": {
				"weekly": {"usedPercent": 45, "secondsRemaining": 3600}
			}
		}
	}`
	usagePct, resetAt, label, details, err := factoryParseLimits([]byte(raw))
	if err != nil {
		t.Fatalf("factoryParseLimits() error: %v", err)
	}
	if usagePct != 45 {
		t.Fatalf("usagePct = %v, want 45", usagePct)
	}
	if label != "Standard weekly" {
		t.Fatalf("label = %q, want Standard weekly", label)
	}
	if resetAt.IsZero() {
		t.Fatal("expected resetAt")
	}
	if details == "" {
		t.Fatal("expected details")
	}
}

func TestZedParseUsage(t *testing.T) {
	raw := `{
		"plan": {
			"name": "Pro",
			"subscriptionPeriod": {"endedAt": "2026-08-01T00:00:00Z"},
			"usage": {"editPredictions": {"used": 25, "limit": 100}}
		}
	}`
	usagePct, resetAt, plan, details, err := zedParseUsage([]byte(raw))
	if err != nil {
		t.Fatalf("zedParseUsage() error: %v", err)
	}
	if usagePct != 25 || plan != "Pro" || resetAt.IsZero() || details == "" {
		t.Fatalf("usagePct=%v plan=%q resetAt=%v details=%q", usagePct, plan, resetAt, details)
	}
}

func TestParseFlexibleTime(t *testing.T) {
	got := parseFlexibleTime("2026-08-01T00:00:00Z")
	if got.IsZero() {
		t.Fatal("expected parsed time")
	}
	if got.Format(time.RFC3339) != "2026-08-01T00:00:00Z" {
		t.Fatalf("unexpected time: %v", got)
	}
}

func TestTitleCase(t *testing.T) {
	if titleCase("standard") != "Standard" {
		t.Fatalf("titleCase() = %q", titleCase("standard"))
	}
}

func TestWindsurfEncodePlanStatusRequest(t *testing.T) {
	payload := windsurfEncodePlanStatusRequest("token")
	if len(payload) == 0 {
		t.Fatal("expected non-empty protobuf payload")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err == nil {
		t.Fatal("expected binary protobuf, not JSON")
	}
}
