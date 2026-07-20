package httphealth

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	HealthOK          = "ok"
	HealthDegraded    = "degraded"
	HealthError       = "error"
	HealthRateLimited = "rate_limited"
	HealthAuthReq     = "auth_required"
)

func ParseRetryAfter(header string) int {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return seconds
	}
	if t, err := http.ParseTime(header); err == nil {
		delta := time.Until(t)
		if delta > 0 {
			return int(delta.Seconds())
		}
	}
	return 0
}

func ClassifyHTTPStatus(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return HealthAuthReq
	case statusCode == http.StatusTooManyRequests:
		return HealthRateLimited
	case statusCode >= 500, statusCode >= 400:
		return HealthDegraded
	default:
		return HealthOK
	}
}

func RefreshAfter(baseSeconds, retryAfterSeconds int) int {
	if retryAfterSeconds > baseSeconds {
		return retryAfterSeconds
	}
	if baseSeconds > 0 {
		return baseSeconds
	}
	return 300
}

func DefaultRefreshAfter(health string, retryAfterSeconds int) int {
	base := 300
	switch health {
	case HealthRateLimited:
		base = 600
	case HealthAuthReq:
		base = 900
	case HealthDegraded, HealthError:
		base = 120
	}
	return RefreshAfter(base, retryAfterSeconds)
}
