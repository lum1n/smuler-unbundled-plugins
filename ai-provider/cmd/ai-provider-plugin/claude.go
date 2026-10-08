package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- Claude (Anthropic) Provider ---
//
// Claude plan usage (5-hour session + weekly windows) is only exposed through
// the undocumented OAuth usage endpoint used by Claude Code itself:
//
//	GET https://api.anthropic.com/api/oauth/usage
//	Authorization: Bearer <Claude.ai OAuth access token, needs user:profile scope>
//	anthropic-beta: oauth-2025-04-20
//
// Tokens come from (in order):
//  1. A token pasted in smuler Settings (raw access token or the Claude Code
//     credentials JSON blob).
//  2. Claude Code's own login: ~/.claude/.credentials.json (or
//     $CLAUDE_CONFIG_DIR/.credentials.json) and, on macOS, the
//     "Claude Code-credentials" Keychain item.
//
// The plugin never refreshes Claude Code's tokens itself: Anthropic rotates
// refresh tokens on use, so refreshing here without writing back would sign
// the user out of Claude Code. When the local token is expired we re-read the
// store (Claude Code refreshes it whenever it runs) and otherwise report
// auth_required.

const (
	// cedar_ember=1 matches what Claude Code and CodexBar request.
	claudeDefaultUsageURL = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
	claudeKeychainService = "Claude Code-credentials"
	claudeDeepLink        = "https://claude.ai/settings/usage"

	// The usage endpoint rate-limits aggressively (persistent 429s are a known
	// issue), so never hit it more than once per claudeMinFetchInterval.
	claudeMinFetchInterval = 60 * time.Second
	claudeMaxBackoff       = 15 * time.Minute
	// Avoid re-running the Keychain lookup (which may show a macOS prompt) on
	// every refresh after it failed.
	claudeLocalRetryInterval = 5 * time.Minute
	claudeKeychainTimeout    = 10 * time.Second
	claudeVersionTimeout     = 5 * time.Second
	// Used when the installed Claude Code version can't be detected.
	claudeFallbackCLIVersion = "2.1.80"
	claudeExpirySkew         = 60 * time.Second
)

var (
	claudeUsageURL = claudeDefaultUsageURL
	// claudeUserAgentOnce resolves the User-Agent lazily (after initialize has
	// set claudeHomeDir, which the version probe needs to find the binary).
	claudeUserAgentOnce  sync.Once
	claudeUserAgentValue string
	// claudeVersionProbe is swappable for tests.
	claudeVersionProbe = detectClaudeCLIVersion
	// claudeHomeDir is set from host.realHome during initialize.
	claudeHomeDir string
	// claudeKeychainReader is swappable for tests.
	claudeKeychainReader = readClaudeKeychain
)

var errClaudeNoCredentials = errors.New("no Claude credentials found")

type claudeCredentials struct {
	AccessToken      string
	ExpiresAt        time.Time
	Scopes           []string
	SubscriptionType string
	Source           string // "settings", "file", "keychain"
}

func (c claudeCredentials) expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && !now.Add(claudeExpirySkew).Before(c.ExpiresAt)
}

func (c claudeCredentials) local() bool { return c.Source == "file" || c.Source == "keychain" }

func (c claudeCredentials) missingProfileScope() bool {
	if len(c.Scopes) == 0 {
		return false // unknown; let the endpoint decide
	}
	for _, s := range c.Scopes {
		if s == "user:profile" {
			return false
		}
	}
	return true
}

// parseClaudeCredentialsJSON parses Claude Code's stored credentials:
// {"claudeAiOauth":{"accessToken":"...","refreshToken":"...","expiresAt":1760000000000,"scopes":[...],"subscriptionType":"max"}}
// The Keychain item may be hex-encoded on some Claude Code versions.
func parseClaudeCredentialsJSON(raw []byte, source string) (claudeCredentials, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return claudeCredentials{}, errClaudeNoCredentials
	}
	if !strings.HasPrefix(trimmed, "{") {
		decoded, err := hex.DecodeString(trimmed)
		if err != nil {
			return claudeCredentials{}, fmt.Errorf("unrecognized Claude credentials format")
		}
		trimmed = strings.TrimSpace(string(decoded))
	}
	var root struct {
		ClaudeAiOauth *struct {
			AccessToken      string          `json:"accessToken"`
			ExpiresAt        json.RawMessage `json:"expiresAt"`
			Scopes           []string        `json:"scopes"`
			SubscriptionType string          `json:"subscriptionType"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(trimmed), &root); err != nil {
		return claudeCredentials{}, fmt.Errorf("Claude credentials parse error")
	}
	if root.ClaudeAiOauth == nil || strings.TrimSpace(root.ClaudeAiOauth.AccessToken) == "" {
		return claudeCredentials{}, errClaudeNoCredentials
	}
	o := root.ClaudeAiOauth
	return claudeCredentials{
		AccessToken:      strings.TrimSpace(o.AccessToken),
		ExpiresAt:        parseClaudeExpiresAt(o.ExpiresAt),
		Scopes:           o.Scopes,
		SubscriptionType: strings.TrimSpace(o.SubscriptionType),
		Source:           source,
	}, nil
}

// parseClaudeExpiresAt accepts epoch milliseconds (Claude Code's format),
// epoch seconds, or an RFC3339 string.
func parseClaudeExpiresAt(raw json.RawMessage) time.Time {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return time.Time{}
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f <= 0 {
			return time.Time{}
		}
		if f > 1e12 {
			return time.UnixMilli(int64(f)).UTC()
		}
		return time.Unix(int64(f), 0).UTC()
	}
	return parseFlexibleTime(s)
}

func claudeCredentialsFilePaths() []string {
	var paths []string
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		paths = append(paths, filepath.Join(dir, ".credentials.json"))
	}
	home := claudeHomeDir
	if home == "" {
		home = geminiHomeDir()
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".claude", ".credentials.json"))
	}
	return paths
}

func readClaudeKeychain(ctx context.Context) ([]byte, error) {
	if runtime.GOOS != "darwin" {
		return nil, errClaudeNoCredentials
	}
	cmdCtx, cancel := context.WithTimeout(ctx, claudeKeychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "/usr/bin/security", "find-generic-password", "-s", claudeKeychainService, "-w")
	cmd.WaitDelay = time.Second
	// Output only captures stdout; never log it (it contains tokens).
	out, err := cmd.Output()
	if err != nil {
		return nil, errClaudeNoCredentials
	}
	return out, nil
}

// loadClaudeLocalCredentials reads Claude Code's credential stores and returns
// the freshest usable entry.
func loadClaudeLocalCredentials(ctx context.Context, now time.Time) (claudeCredentials, error) {
	var candidates []claudeCredentials
	for _, path := range claudeCredentialsFilePaths() {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if c, err := parseClaudeCredentialsJSON(body, "file"); err == nil {
			candidates = append(candidates, c)
			if !c.expired(now) {
				return c, nil
			}
		}
	}
	if body, err := claudeKeychainReader(ctx); err == nil {
		if c, err := parseClaudeCredentialsJSON(body, "keychain"); err == nil {
			candidates = append(candidates, c)
		}
	}
	if len(candidates) == 0 {
		return claudeCredentials{}, errClaudeNoCredentials
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.ExpiresAt.After(best.ExpiresAt) {
			best = c
		}
	}
	return best, nil
}

// --- Usage response ---

type claudeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeUsageResponse struct {
	FiveHour       *claudeUsageWindow `json:"five_hour"`
	SevenDay       *claudeUsageWindow `json:"seven_day"`
	SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
}

func parseClaudeUsage(body []byte, plan string, now time.Time) (ProviderStatus, error) {
	var usage claudeUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return ProviderStatus{}, fmt.Errorf("Claude parse error: %w", err)
	}
	type window struct {
		label   string
		pct     float64
		resetAt time.Time
	}
	var windows []window
	for _, w := range []struct {
		label string
		data  *claudeUsageWindow
	}{
		{"Session", usage.FiveHour},
		{"Weekly", usage.SevenDay},
		{"Opus weekly", usage.SevenDayOpus},
		{"Sonnet weekly", usage.SevenDaySonnet},
	} {
		if w.data == nil || w.data.Utilization == nil {
			continue
		}
		var resetAt time.Time
		if w.data.ResetsAt != nil {
			resetAt = parseFlexibleTime(*w.data.ResetsAt)
		}
		windows = append(windows, window{w.label, max(0, min(100, *w.data.Utilization)), resetAt})
	}
	if len(windows) == 0 {
		return ProviderStatus{}, fmt.Errorf("Claude usage response had no usage windows")
	}

	primary := windows[0]
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		if w.pct > primary.pct {
			primary = w
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", w.label, w.pct))
	}
	summary := strings.Join(parts, " · ")
	details := summary
	if !primary.resetAt.IsZero() {
		details += ", resets in " + durationUntil(primary.resetAt)
	}
	windowLabel := primary.label + " usage"
	if plan != "" {
		windowLabel = titleCase(plan) + " · " + windowLabel
	}
	return ProviderStatus{
		ProviderID:     "claude",
		DisplayName:    "Claude",
		UsagePercent:   primary.pct,
		RemainingLabel: summary,
		WindowLabel:    windowLabel,
		ResetAt:        primary.resetAt,
		Severity:       severityForPercent(primary.pct),
		Health:         "ready",
		DeepLink:       claudeDeepLink,
		SummaryValue:   fmt.Sprintf("%.0f%%", primary.pct),
		Details:        details,
		Timestamp:      now.UTC(),
	}, nil
}

// --- Provider with per-account cache/backoff state ---

type claudeAccountState struct {
	lastGood     *ProviderStatus
	lastGoodAt   time.Time
	lastFetchAt  time.Time
	backoffUntil time.Time
	backoffStep  int

	localCreds        *claudeCredentials
	localReadFailedAt time.Time
}

type claudeProvider struct {
	mu     sync.Mutex
	states map[string]*claudeAccountState
	now    func() time.Time
}

func (p *claudeProvider) ID() string          { return "claude" }
func (p *claudeProvider) DisplayName() string { return "Claude" }

func (p *claudeProvider) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *claudeProvider) state(key string) *claudeAccountState {
	if p.states == nil {
		p.states = make(map[string]*claudeAccountState)
	}
	st, ok := p.states[key]
	if !ok {
		st = &claudeAccountState{}
		p.states[key] = st
	}
	return st
}

func claudeStatus(health, details string, now time.Time) ProviderStatus {
	return ProviderStatus{
		ProviderID:  "claude",
		DisplayName: "Claude",
		Health:      health,
		Severity:    "info",
		WindowLabel: "Claude",
		DeepLink:    claudeDeepLink,
		Details:     details,
		Timestamp:   now.UTC(),
	}
}

// settingsCredentials interprets a token pasted in smuler Settings.
func claudeSettingsCredentials(auth AuthContext) (claudeCredentials, bool, error) {
	raw := strings.TrimSpace(auth.AccessToken)
	if raw == "" {
		raw = strings.TrimSpace(auth.APIKey)
	}
	if raw == "" {
		return claudeCredentials{}, false, nil
	}
	if strings.HasPrefix(raw, "{") {
		c, err := parseClaudeCredentialsJSON([]byte(raw), "settings")
		return c, err == nil, err
	}
	if strings.HasPrefix(raw, "sk-ant-api") {
		return claudeCredentials{}, false, fmt.Errorf("Anthropic API keys cannot read Claude plan usage")
	}
	c := claudeCredentials{AccessToken: raw, Source: "settings"}
	if exp := parseFlexibleTime(auth.ExpiresAt); !exp.IsZero() {
		c.ExpiresAt = exp
	}
	return c, true, nil
}

// resolveCredentials must be called with p.mu held.
func (p *claudeProvider) resolveCredentials(ctx context.Context, auth AuthContext, st *claudeAccountState, forceReload bool) (claudeCredentials, error) {
	now := p.clock()
	settingsCreds, ok, settingsErr := claudeSettingsCredentials(auth)
	if ok && !settingsCreds.expired(now) {
		return settingsCreds, nil
	}

	if !forceReload && st.localCreds != nil && !st.localCreds.expired(now) {
		return *st.localCreds, nil
	}
	if !forceReload && st.localCreds == nil && !st.localReadFailedAt.IsZero() && now.Sub(st.localReadFailedAt) < claudeLocalRetryInterval {
		if ok {
			return settingsCreds, nil // expired; reported by caller
		}
		if settingsErr != nil {
			return claudeCredentials{}, settingsErr
		}
		return claudeCredentials{}, errClaudeNoCredentials
	}

	local, err := loadClaudeLocalCredentials(ctx, now)
	if err != nil {
		st.localCreds = nil
		st.localReadFailedAt = now
		if ok {
			return settingsCreds, nil
		}
		if settingsErr != nil {
			return claudeCredentials{}, settingsErr
		}
		return claudeCredentials{}, err
	}
	st.localCreds = &local
	st.localReadFailedAt = time.Time{}
	return local, nil
}

// claudeUserAgent returns the User-Agent for the usage endpoint. The endpoint
// rate-limits non-Claude-Code clients aggressively, so like CodexBar we send
// Claude Code's own format with the installed CLI version:
// "claude-cli/<version> (external, cli)". SMULER_CLAUDE_USER_AGENT overrides it.
func claudeUserAgent() string {
	claudeUserAgentOnce.Do(func() {
		if ua := strings.TrimSpace(os.Getenv("SMULER_CLAUDE_USER_AGENT")); ua != "" {
			claudeUserAgentValue = ua
			return
		}
		version := claudeVersionProbe()
		if version == "" {
			version = claudeFallbackCLIVersion
		}
		claudeUserAgentValue = "claude-cli/" + version + " (external, cli)"
	})
	return claudeUserAgentValue
}

// detectClaudeCLIVersion runs `claude --version` (output like
// "2.1.80 (Claude Code)") and returns the version, or "" if unavailable.
// GUI-launched processes have a minimal PATH, so common install locations are
// probed as well.
func detectClaudeCLIVersion() string {
	candidates := []string{}
	if path, err := exec.LookPath("claude"); err == nil {
		candidates = append(candidates, path)
	}
	if claudeHomeDir != "" {
		candidates = append(candidates,
			filepath.Join(claudeHomeDir, ".local", "bin", "claude"),
			filepath.Join(claudeHomeDir, ".claude", "local", "claude"),
		)
	}
	candidates = append(candidates, "/opt/homebrew/bin/claude", "/usr/local/bin/claude")

	for _, bin := range candidates {
		if info, err := os.Stat(bin); err != nil || info.IsDir() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), claudeVersionTimeout)
		cmd := exec.CommandContext(ctx, bin, "--version")
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		cancel()
		if err != nil {
			continue
		}
		if v := parseClaudeCLIVersion(string(out)); v != "" {
			return v
		}
	}
	return ""
}

// parseClaudeCLIVersion extracts "2.1.80" from "2.1.80 (Claude Code)".
func parseClaudeCLIVersion(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return ""
	}
	v := strings.TrimPrefix(fields[0], "v")
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			return ""
		}
	}
	if !strings.Contains(v, ".") {
		return ""
	}
	return v
}

type claudeHTTPResult struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

func claudeFetchUsage(ctx context.Context, token string) (claudeHTTPResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return claudeHTTPResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claudeUserAgent())

	resp, err := httpClient.Do(req)
	if err != nil {
		return claudeHTTPResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return claudeHTTPResult{}, err
	}
	return claudeHTTPResult{
		status:     resp.StatusCode,
		body:       body,
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
	}, nil
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func (p *claudeProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	key := auth.AccountID
	if key == "" {
		key = "default"
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state(key)
	now := p.clock()

	if st.lastGood != nil && now.Sub(st.lastFetchAt) < claudeMinFetchInterval && st.backoffUntil.IsZero() {
		return *st.lastGood, nil
	}
	if now.Before(st.backoffUntil) {
		return p.rateLimitedStatus(st, now), nil
	}

	creds, err := p.resolveCredentials(ctx, auth, st, false)
	if err != nil {
		msg := "Sign in with Claude Code (run `claude`, then /login), or paste a Claude OAuth token in Settings."
		if !errors.Is(err, errClaudeNoCredentials) {
			msg = err.Error() + ". Remove it to use your Claude Code login, or paste an OAuth token."
		}
		return claudeStatus("auth_required", msg, now), nil
	}
	if creds.expired(now) && creds.local() {
		// Claude Code may have refreshed since we cached it.
		if fresh, rerr := p.resolveCredentials(ctx, auth, st, true); rerr == nil {
			creds = fresh
		}
	}
	if creds.expired(now) {
		if creds.local() {
			return claudeStatus("auth_required", "Claude Code login expired. Run `claude` once to refresh it.", now), nil
		}
		return claudeStatus("auth_required", "Claude token expired. Reconnect in Settings.", now), nil
	}
	if creds.missingProfileScope() {
		return claudeStatus("auth_required", "Claude token lacks the user:profile scope needed for usage. Sign in again with `claude` /login.", now), nil
	}

	st.lastFetchAt = now
	res, err := claudeFetchUsage(ctx, creds.AccessToken)
	if err != nil {
		return ProviderStatus{}, err
	}

	if res.status == http.StatusUnauthorized && creds.local() {
		// Token may have been rotated by Claude Code; re-read once and retry.
		if fresh, rerr := p.resolveCredentials(ctx, auth, st, true); rerr == nil && fresh.AccessToken != creds.AccessToken {
			creds = fresh
			if res, err = claudeFetchUsage(ctx, creds.AccessToken); err != nil {
				return ProviderStatus{}, err
			}
		}
	}

	switch {
	case res.status == http.StatusOK:
		status, err := parseClaudeUsage(res.body, creds.SubscriptionType, p.clock())
		if err != nil {
			return ProviderStatus{}, err
		}
		st.lastGood = &status
		st.lastGoodAt = now
		st.backoffUntil = time.Time{}
		st.backoffStep = 0
		return status, nil
	case res.status == http.StatusUnauthorized:
		st.localCreds = nil
		if creds.local() {
			return claudeStatus("auth_required", "Claude Code token was rejected. Run `claude` and /login again.", now), nil
		}
		return claudeStatus("auth_required", "Claude token was rejected. Reconnect in Settings.", now), nil
	case res.status == http.StatusForbidden && strings.Contains(strings.ToLower(string(res.body)), "scope"):
		return claudeStatus("auth_required", "Claude token lacks the user:profile scope needed for usage. Sign in again with `claude` /login.", now), nil
	case res.status == http.StatusTooManyRequests:
		delay := res.retryAfter
		if delay <= 0 {
			delay = time.Minute << min(st.backoffStep, 4) // 1, 2, 4, 8, 16 min
		}
		delay = min(delay, claudeMaxBackoff)
		st.backoffStep++
		st.backoffUntil = now.Add(delay)
		return p.rateLimitedStatus(st, now), nil
	default:
		return ProviderStatus{}, formatHTTPError("Claude", res.status, res.body)
	}
}

// rateLimitedStatus serves the last good reading (marked stale) while backing
// off, or a rate_limited card when there is none.
func (p *claudeProvider) rateLimitedStatus(st *claudeAccountState, now time.Time) ProviderStatus {
	wait := durationUntilFrom(st.backoffUntil, now)
	if st.lastGood != nil {
		stale := *st.lastGood
		stale.Details = fmt.Sprintf("%s (rate limited, last updated %s ago; retry in %s)",
			st.lastGood.Details, durationSince(st.lastGoodAt, now), wait)
		return stale
	}
	return claudeStatus("rate_limited", "Claude usage API rate limited. Retrying in "+wait+".", now)
}

func durationUntilFrom(t, now time.Time) string {
	d := t.Sub(now)
	if d < time.Minute {
		return "<1m"
	}
	return durationUntil(time.Now().Add(d))
}

func durationSince(t, now time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return "<1m"
	}
	return durationUntil(time.Now().Add(d))
}
