package httphealth

import "testing"

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		code   int
		health string
	}{
		{200, HealthOK},
		{401, HealthAuthReq},
		{403, HealthAuthReq},
		{429, HealthRateLimited},
		{500, HealthDegraded},
		{404, HealthDegraded},
	}
	for _, tc := range tests {
		if got := ClassifyHTTPStatus(tc.code); got != tc.health {
			t.Fatalf("status %d: got %q want %q", tc.code, got, tc.health)
		}
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	if got := ParseRetryAfter("120"); got != 120 {
		t.Fatalf("got %d want 120", got)
	}
}

func TestDefaultRefreshAfterUsesRetryAfter(t *testing.T) {
	if got := DefaultRefreshAfter(HealthRateLimited, 900); got != 900 {
		t.Fatalf("got %d want 900", got)
	}
}
