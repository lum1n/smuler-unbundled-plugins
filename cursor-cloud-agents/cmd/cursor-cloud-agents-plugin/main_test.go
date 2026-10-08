package main

import (
	"testing"

	"github.com/lum1n/smuler/plugins/cursor-cloud-agents/internal/cursorapi"
	"github.com/lum1n/smuler/plugins/sdk-go"
)

func TestTruncateIsRuneSafe(t *testing.T) {
	got := truncate("ææææææ", 4)
	if got != "æææ…" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestErrorSnapshotRateLimitHonorsRetryAfter(t *testing.T) {
	snap := errorSnapshot(&cursorapi.APIError{StatusCode: 429, RetryAfter: 900}, 60)
	if snap.Health != sdk.HealthRateLimited || snap.RefreshAfter != 900 {
		t.Fatalf("health=%s refreshAfter=%d", snap.Health, snap.RefreshAfter)
	}
	snap = errorSnapshot(&cursorapi.APIError{StatusCode: 429}, 600)
	if snap.RefreshAfter != 600 {
		t.Fatalf("rate limit shortened refresh to %d", snap.RefreshAfter)
	}
}

func TestOpenActionResolvesItemID(t *testing.T) {
	h := &handler{itemURLs: map[string]string{"bc-1": ""}}
	if ok, msg := h.PerformAction("open", map[string]string{"id": "bc-1"}); ok || msg != "missing url" {
		t.Fatalf("expected missing url for empty mapping, got %v %q", ok, msg)
	}
}
