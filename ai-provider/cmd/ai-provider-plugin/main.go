package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/plugindebug"
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
			Resolver: &net.Resolver{
				PreferGo: true,
			},
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
	Timeout: 20 * time.Second,
}

func logDebug(format string, args ...interface{}) {
	plugindebug.Log("[ai-provider-plugin]", format, args...)
}

const (
	protocolVersion = "0.1.0"
	pluginVersion   = "0.1.1"
	pluginID        = "ai-provider"
)

// --- JSON-RPC types ---

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    *pluginErrorData `json:"data,omitempty"`
}

type pluginErrorData struct {
	Retryable       bool   `json:"retryable"`
	SuggestedAction string `json:"suggestedAction,omitempty"`
}

// --- Initialize / Auth types ---

type initializeParams struct {
	ProtocolVersion string                `json:"protocolVersion"`
	PluginID        string                `json:"pluginId"`
	Config          map[string]string     `json:"config"`
	Auth            *authContext          `json:"auth"`          // backward compat
	ProviderAuths   []providerAuthContext `json:"providerAuths"` // new multi-provider
}

type authContext struct {
	AccountID string `json:"accountId"`
}

type providerAuthContext struct {
	ProviderID   string `json:"providerId"`
	Kind         string `json:"kind"`
	AccountID    string `json:"accountId,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	APIKey       string `json:"apiKey,omitempty"`
	CookieHeader string `json:"cookieHeader,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	DisplayName  string `json:"displayName,omitempty"`
}

type initializedPayload struct {
	Type            string `json:"type"`
	ProtocolVersion string `json:"protocolVersion"`
	PluginVersion   string `json:"pluginVersion"`
	Health          string `json:"health"`
}

// --- Snapshot types ---

type pluginSnapshot struct {
	PluginID     string         `json:"pluginId"`
	State        string         `json:"state"`
	Summary      pluginSummary  `json:"summary"`
	Items        []pluginItem   `json:"items"`
	Actions      []pluginAction `json:"actions"`
	Alerts       []pluginAlert  `json:"alerts"`
	RefreshAfter int            `json:"refreshAfter"`
	Health       string         `json:"health"`
}

type pluginSummary struct {
	Title    string `json:"title"`
	Value    string `json:"value"`
	Trend    string `json:"trend"`
	Severity string `json:"severity"`
	IconHint string `json:"iconHint"`
}

type pluginItem struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Subtitle  string            `json:"subtitle,omitempty"`
	Detail    string            `json:"detail,omitempty"`
	Severity  string            `json:"severity"`
	Timestamp string            `json:"timestamp,omitempty"`
	DeepLink  string            `json:"deepLink,omitempty"`
	Actions   []pluginAction    `json:"actions"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type pluginAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type pluginAlert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type pluginEvent struct {
	Type      string            `json:"type"`
	PluginID  string            `json:"pluginId"`
	Message   string            `json:"message"`
	Severity  string            `json:"severity"`
	Data      map[string]string `json:"data"`
	Timestamp string            `json:"timestamp"`
}

type eventParams struct {
	Event pluginEvent `json:"event"`
}

// --- Provider interface ---

type AuthContext struct {
	ProviderID   string
	Kind         string
	AccountID    string
	AccessToken  string
	APIKey       string
	CookieHeader string
	ExpiresAt    string
	DisplayName  string
}

type ProviderStatus struct {
	ProviderID         string
	DisplayName        string
	AccountID          string
	AccountDisplayName string
	UsagePercent       float64
	RemainingLabel     string
	WindowLabel        string
	ResetAt            time.Time
	Severity           string
	Health             string
	DeepLink           string
	SummaryValue       string
	Details            string
	Timestamp          time.Time
}

type Provider interface {
	ID() string
	DisplayName() string
	Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error)
}

// --- Provider config ---

type aiProviderConfig struct {
	EnabledProviders      []string
	WarningThreshold      float64
	CriticalThreshold     float64
	CopilotAccountType    string
	CopilotOrgName        string
	CopilotEnterpriseHost string
	DevinOrgName          string
}

func defaultConfig() aiProviderConfig {
	return aiProviderConfig{
		// No providers enabled until the user opts in via Settings.
		EnabledProviders:   []string{},
		WarningThreshold:   75.0,
		CriticalThreshold:  90.0,
		CopilotAccountType: "personal",
	}
}

func parseConfig(cfg map[string]string) aiProviderConfig {
	c := defaultConfig()
	if v, ok := cfg["enabledProviders"]; ok {
		c.EnabledProviders = splitCSV(v)
	}
	if v, ok := cfg["warningThreshold"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 100 {
			c.WarningThreshold = f
		}
	}
	if v, ok := cfg["criticalThreshold"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 100 {
			c.CriticalThreshold = f
		}
	}
	if v, ok := cfg["copilotAccountType"]; ok {
		c.CopilotAccountType = strings.TrimSpace(v)
		copilotAccountType = c.CopilotAccountType
	}
	if v, ok := cfg["copilotOrgName"]; ok {
		c.CopilotOrgName = strings.TrimSpace(v)
		copilotOrgName = c.CopilotOrgName
	}
	if v, ok := cfg["copilotEnterpriseHost"]; ok {
		c.CopilotEnterpriseHost = strings.TrimSpace(v)
		copilotEnterpriseHost = c.CopilotEnterpriseHost
	}
	if v, ok := cfg["devinOrgName"]; ok {
		c.DevinOrgName = strings.TrimSpace(v)
		devinOrgName = c.DevinOrgName
	}
	return c
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// --- AI Provider Plugin ---

type aiProviderPlugin struct {
	client       *http.Client
	config       aiProviderConfig
	providerAuth map[string][]AuthContext
	lastSnapshot *pluginSnapshot
	prevUsage    map[string]float64 // providerID -> previous usage percent
}

// --- Provider priority for tie-breaking ---

var providerPriority = map[string]int{
	"codex":        0,
	"claude":       1,
	"cursor":       2,
	"commandcode":  3,
	"gemini":       4,
	"grok":         5,
	"windsurf":     6,
	"opencode-go":  7,
	"opencode":     8,
	"openrouter":   9,
	"copilot":      10,
	"augment":      11,
	"factory":      12,
	"zed":          13,
	"warp":         14,
	"devin":        15,
	"kiro":         16,
}

var copilotAccountType = "personal"
var copilotOrgName = ""
var copilotEnterpriseHost = ""

// --- OpenRouter Provider ---

type openRouterProvider struct{}

func (p *openRouterProvider) ID() string          { return "openrouter" }
func (p *openRouterProvider) DisplayName() string { return "OpenRouter" }

type openRouterCreditsResponse struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

func (p *openRouterProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	token := strings.TrimSpace(authToken(auth))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/credits", nil)
	if err != nil {
		return ProviderStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.0")

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return ProviderStatus{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProviderStatus{}, err
	}

	if resp.StatusCode >= 400 {
		return ProviderStatus{}, formatHTTPError("OpenRouter", resp.StatusCode, body)
	}

	var credits openRouterCreditsResponse
	if err := json.Unmarshal(body, &credits); err != nil {
		return ProviderStatus{}, fmt.Errorf("OpenRouter parse error: %w", err)
	}

	total := credits.Data.TotalCredits
	used := credits.Data.TotalUsage
	remaining := total - used

	var usagePercent float64
	if total > 0 {
		usagePercent = (used / total) * 100.0
	}

	var summaryValue string
	if total > 0 {
		summaryValue = fmt.Sprintf("%.0f%%", usagePercent)
	} else {
		summaryValue = fmt.Sprintf("$%.2f left", remaining)
	}

	return ProviderStatus{
		ProviderID:     "openrouter",
		DisplayName:    "OpenRouter",
		UsagePercent:   usagePercent,
		RemainingLabel: fmt.Sprintf("$%.2f / $%.2f", remaining, total),
		WindowLabel:    "Credit balance",
		Severity:       severityForPercent(usagePercent),
		Health:         "ready",
		DeepLink:       "https://openrouter.ai/credits",
		SummaryValue:   summaryValue,
		Details:        fmt.Sprintf("$%.2f remaining", remaining),
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Claude (Anthropic) Provider ---

type claudeProvider struct{}

func (p *claudeProvider) ID() string          { return "claude" }
func (p *claudeProvider) DisplayName() string { return "Claude" }

type anthropicUsageResponse struct {
	UsagePct    float64 `json:"usage_pct"`
	Limit       int     `json:"limit"`
	Used        int     `json:"used"`
	ResetAt     string  `json:"reset_at"`
	WindowLabel string  `json:"window_label"`
}

func (p *claudeProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://console.anthropic.com/api/usage", nil)
	if err != nil {
		return ProviderStatus{}, err
	}
	req.Header.Set("x-api-key", auth.APIKey)
	if auth.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	}
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.0")

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return ProviderStatus{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProviderStatus{}, err
	}

	if resp.StatusCode == 404 {
		return ProviderStatus{
			ProviderID:  "claude",
			DisplayName: "Claude",
			Health:      "degraded",
			Severity:    "info",
			Details:     "Usage endpoint not yet available",
			WindowLabel: "Usage",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	if resp.StatusCode >= 400 {
		return ProviderStatus{}, fmt.Errorf("Claude API error (%d): %s", resp.StatusCode, string(body))
	}

	var usage anthropicUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return ProviderStatus{}, fmt.Errorf("Claude parse error: %w", err)
	}

	var resetAt time.Time
	windowLabel := "Weekly usage"
	if usage.ResetAt != "" {
		resetAt, _ = time.Parse(time.RFC3339, usage.ResetAt)
	} else {
		resetAt = nextSaturday()
	}
	if usage.WindowLabel != "" {
		windowLabel = usage.WindowLabel
	}

	severity := severityForPercent(usage.UsagePct)
	summaryValue := fmt.Sprintf("%.0f%%", usage.UsagePct)

	var details string
	if !resetAt.IsZero() {
		details = fmt.Sprintf("%.0f%% used, resets in %s", usage.UsagePct, durationUntil(resetAt))
	} else {
		details = fmt.Sprintf("%.0f%% used", usage.UsagePct)
	}

	return ProviderStatus{
		ProviderID:     "claude",
		DisplayName:    "Claude",
		UsagePercent:   usage.UsagePct,
		RemainingLabel: fmt.Sprintf("%d / %d", usage.Used, usage.Limit),
		WindowLabel:    windowLabel,
		ResetAt:        resetAt,
		Severity:       severity,
		Health:         "ready",
		DeepLink:       "https://console.anthropic.com/settings/usage",
		SummaryValue:   summaryValue,
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Codex Provider ---

type codexProvider struct{}

func (p *codexProvider) ID() string          { return "codex" }
func (p *codexProvider) DisplayName() string { return "Codex" }

type openAIUsageResponse struct {
	TotalUsage float64 `json:"total_usage"`
	HardLimit  float64 `json:"hard_limit_usd"`
	SoftLimit  float64 `json:"soft_limit_usd"`
}

func (p *codexProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.openai.com/v1/usage?date="+time.Now().UTC().Format("2006-01-02"), nil)
	if err != nil {
		return ProviderStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+authToken(auth))
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.0")

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return ProviderStatus{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProviderStatus{}, err
	}

	if resp.StatusCode == 404 {
		return ProviderStatus{
			ProviderID:  "codex",
			DisplayName: "Codex",
			Health:      "degraded",
			Severity:    "info",
			Details:     "Usage endpoint not yet available",
			WindowLabel: "Usage",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	if resp.StatusCode >= 400 {
		return ProviderStatus{}, fmt.Errorf("Codex API error (%d): %s", resp.StatusCode, string(body))
	}

	var usage openAIUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return ProviderStatus{}, fmt.Errorf("Codex parse error: %w", err)
	}

	limit := usage.HardLimit
	if limit <= 0 {
		limit = usage.SoftLimit
	}
	var usagePercent float64
	if limit > 0 {
		usagePercent = (usage.TotalUsage / limit) * 100.0
	}
	resetAt := endOfMonth()

	severity := severityForPercent(usagePercent)
	summaryValue := fmt.Sprintf("%.0f%%", usagePercent)

	return ProviderStatus{
		ProviderID:     "codex",
		DisplayName:    "Codex",
		UsagePercent:   usagePercent,
		RemainingLabel: fmt.Sprintf("$%.2f / $%.2f", limit-usage.TotalUsage, limit),
		WindowLabel:    "Monthly usage",
		ResetAt:        resetAt,
		Severity:       severity,
		Health:         "ready",
		DeepLink:       "https://platform.openai.com/usage",
		SummaryValue:   summaryValue,
		Details:        fmt.Sprintf("%.0f%% used, resets in %s", usagePercent, durationUntil(resetAt)),
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- OpenCode shared helpers ---

func getEnvOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var (
	opencodeBaseURL     string
	opencodeServerURL   string
	opencodeWorkspaceID string
	opencodeBillingID   string
	opencodeUserAgent   string
)

func init() {
	opencodeBaseURL = getEnvOrDefault("SMULER_OPENCODE_BASE_URL", "https://opencode.ai")
	opencodeServerURL = getEnvOrDefault("SMULER_OPENCODE_SERVER_URL", "https://opencode.ai/_server")
	opencodeWorkspaceID = getEnvOrDefault("SMULER_OPENCODE_WORKSPACE_ID", "def39973159c7f0483d8793a822b8dbb10d067e12c65455fcb4608459ba0234f")
	opencodeBillingID = getEnvOrDefault("SMULER_OPENCODE_BILLING_ID", "7abeebee372f304e050aaaf92be863f4a86490e382f8c79db68fd94040d691b4")
	opencodeUserAgent = getEnvOrDefault("SMULER_OPENCODE_USER_AGENT", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
}

func opencodeCookieHeader(rawCookie string) string {
	var result []string
	attributeNames := map[string]struct{}{
		"path":     {},
		"domain":   {},
		"expires":  {},
		"max-age":  {},
		"samesite": {},
		"secure":   {},
		"httponly": {},
	}
	for _, pair := range strings.Split(rawCookie, ";") {
		trimmed := strings.TrimSpace(pair)
		if trimmed == "" {
			continue
		}

		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			if _, ok := attributeNames[strings.ToLower(trimmed)]; ok {
				continue
			}
			continue
		}

		name := strings.TrimSpace(parts[0])
		if name == "" {
			continue
		}
		if _, ok := attributeNames[strings.ToLower(name)]; ok {
			continue
		}

		result = append(result, trimmed)
	}
	return strings.Join(result, "; ")
}

func opencodeServerRequest(ctx context.Context, serverID string, args string, cookieHeader string, referer string, method string) (*http.Response, []byte, error) {
	var reqURL string
	if method == "GET" {
		q := url.Values{}
		q.Set("id", serverID)
		if args != "" {
			q.Set("args", args)
		}
		reqURL = fmt.Sprintf("%s?%s", opencodeServerURL, q.Encode())
	} else {
		reqURL = opencodeServerURL
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, nil, err
	}

	cookie := opencodeCookieHeader(cookieHeader)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("X-Server-Id", serverID)
	req.Header.Set("X-Server-Instance", "server-fn:"+uuidString())
	req.Header.Set("User-Agent", opencodeUserAgent)
	req.Header.Set("Origin", opencodeBaseURL)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "text/javascript, application/json;q=0.9, */*;q=0.8")

	if method != "GET" && args != "" {
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(args))
	}

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}

	return resp, body, nil
}

func opencodeFetchPage(ctx context.Context, pageURL string, cookieHeader string, providerName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	cookie := opencodeCookieHeader(cookieHeader)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("User-Agent", opencodeUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	client := httpClient
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	text := string(body)
	if strings.Contains(strings.ToLower(text), "login") || strings.Contains(strings.ToLower(text), "sign in") {
		return "", fmt.Errorf("session expired or invalid")
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", fmt.Errorf("session expired or invalid")
	}
	if resp.StatusCode != 200 {
		return "", formatHTTPError(providerName, resp.StatusCode, body)
	}
	return text, nil
}

func opencodeFetchWorkspaceID(ctx context.Context, cookieHeader string) (string, error) {
	// Try GET first
	_, body, err := opencodeServerRequest(ctx, opencodeWorkspaceID, "", cookieHeader, opencodeBaseURL, "GET")
	if err != nil {
		return "", err
	}

	text := string(body)
	if strings.Contains(strings.ToLower(text), "login") || strings.Contains(strings.ToLower(text), "sign in") {
		return "", fmt.Errorf("session expired or invalid")
	}

	ids := opencodeExtractWorkspaceIDs(text)
	if len(ids) == 0 {
		// Try POST fallback
		_, body, err = opencodeServerRequest(ctx, opencodeWorkspaceID, "[]", cookieHeader, opencodeBaseURL, "POST")
		if err != nil {
			return "", err
		}
		text = string(body)
		ids = opencodeExtractWorkspaceIDs(text)
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no workspace found")
	}
	return ids[0], nil
}

func opencodeExtractWorkspaceIDs(text string) []string {
	var ids []string
	start := 0
	for {
		idx := strings.Index(text[start:], "wrk_")
		if idx < 0 {
			break
		}
		absIdx := start + idx
		end := absIdx + 4
		for end < len(text) && (text[end] >= 'a' && text[end] <= 'z' || text[end] >= 'A' && text[end] <= 'Z' || text[end] >= '0' && text[end] <= '9') {
			end++
		}
		id := text[absIdx:end]
		if len(id) > 4 {
			ids = append(ids, id)
		}
		start = absIdx + len(id)
	}
	return ids
}

func opencodeExtractFloat(text string, pattern string) (float64, bool) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0, false
	}
	match := re.FindStringSubmatch(text)
	if len(match) >= 2 {
		v, err := strconv.ParseFloat(match[1], 64)
		if err == nil {
			return v, true
		}
	}
	return 0, false
}

func opencodeExtractInt(text string, pattern string) (int, bool) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return 0, false
	}
	match := re.FindStringSubmatch(text)
	if len(match) >= 2 {
		v, err := strconv.Atoi(match[1])
		if err == nil {
			return v, true
		}
	}
	return 0, false
}

// opencodePercent clamps an already-percentage usage value to [0, 100].
// OpenCode fields like usagePercent are on a 0–100 scale (e.g. 1 means 1%),
// so values in (0, 1] must not be treated as fractions.
func opencodePercent(raw float64) float64 {
	if raw < 0 {
		return 0
	}
	if raw > 100 {
		return 100
	}
	return raw
}

func uuidString() string {
	b := make([]byte, 8)
	n := time.Now().UnixNano()
	for i := range b {
		b[i] = byte(n >> (i * 8))
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[0:2], b[2:8])
}

// --- OpenCode Provider ---

type openCodeProvider struct{}

func (p *openCodeProvider) ID() string          { return "opencode" }
func (p *openCodeProvider) DisplayName() string { return "OpenCode" }

func (p *openCodeProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	cookie := strings.TrimSpace(auth.CookieHeader)
	if cookie == "" {
		return ProviderStatus{}, fmt.Errorf("no session cookie")
	}

	workspaceID, err := opencodeFetchWorkspaceID(ctx, cookie)
	if err != nil {
		return ProviderStatus{
			ProviderID:  "opencode",
			DisplayName: "OpenCode",
			Health:      "auth_required",
			Severity:    "info",
			Details:     "Session expired or invalid. Re-import from browser.",
			WindowLabel: "Usage",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	referer := fmt.Sprintf("%s/workspace/%s/billing", opencodeBaseURL, workspaceID)
	args := fmt.Sprintf("[%q]", workspaceID)

	resp, body, err := opencodeServerRequest(ctx, opencodeBillingID, args, cookie, referer, "GET")
	if err != nil {
		return ProviderStatus{}, err
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ProviderStatus{
			ProviderID:  "opencode",
			DisplayName: "OpenCode",
			Health:      "auth_required",
			Severity:    "info",
			Details:     "Session expired or invalid. Re-import from browser.",
			WindowLabel: "Usage",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	text := string(body)
	if strings.Contains(strings.ToLower(text), "login") || strings.Contains(strings.ToLower(text), "sign in") {
		return ProviderStatus{
			ProviderID:  "opencode",
			DisplayName: "OpenCode",
			Health:      "auth_required",
			Severity:    "info",
			Details:     "Session expired or invalid. Re-import from browser.",
			WindowLabel: "Usage",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	if resp.StatusCode != 200 {
		resp, body, err = opencodeServerRequest(ctx, opencodeBillingID, args, cookie, referer, "POST")
		if err != nil {
			return ProviderStatus{}, err
		}
		if resp.StatusCode != 200 {
			return ProviderStatus{}, formatHTTPError("OpenCode", resp.StatusCode, body)
		}
		text = string(body)
	}

	var data map[string]interface{}
	var rollingPct, weeklyPct float64
	var rollingResetSec, weeklyResetSec int

	if json.Unmarshal(body, &data) == nil {
		rollingPct, rollingResetSec = opencodeExtractWindow(data, "rolling")
		weeklyPct, weeklyResetSec = opencodeExtractWindow(data, "weekly")
	}

	if rollingPct == 0 && weeklyPct == 0 {
		rollingPct, _ = opencodeExtractFloat(text, `rollingUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
		rollingResetSec, _ = opencodeExtractInt(text, `rollingUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
		weeklyPct, _ = opencodeExtractFloat(text, `weeklyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
		weeklyResetSec, _ = opencodeExtractInt(text, `weeklyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
	}

	rollingPct = opencodePercent(rollingPct)
	weeklyPct = opencodePercent(weeklyPct)

	usagePct := rollingPct
	if weeklyPct > usagePct {
		usagePct = weeklyPct
	}
	resetSec := rollingResetSec
	if weeklyResetSec < resetSec || resetSec == 0 {
		resetSec = weeklyResetSec
	}
	resetAt := time.Now().Add(time.Duration(resetSec) * time.Second)

	severity := severityForPercent(usagePct)

	var details string
	if resetSec > 0 {
		details = fmt.Sprintf("Rolling %.0f%%, Weekly %.0f%%, resets in %s", rollingPct, weeklyPct, durationUntil(resetAt))
	} else {
		details = fmt.Sprintf("Rolling %.0f%%, Weekly %.0f%%", rollingPct, weeklyPct)
	}

	return ProviderStatus{
		ProviderID:     "opencode",
		DisplayName:    "OpenCode",
		UsagePercent:   usagePct,
		RemainingLabel: fmt.Sprintf("R %.0f%% / W %.0f%%", rollingPct, weeklyPct),
		WindowLabel:    "Rolling + Weekly",
		ResetAt:        resetAt,
		Severity:       severity,
		Health:         "ready",
		DeepLink:       fmt.Sprintf("https://opencode.ai/workspace/%s/billing", workspaceID),
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

func opencodeExtractWindow(data map[string]interface{}, kind string) (float64, int) {
	keys := []string{kind + "Usage", kind, kind + "_usage", kind + "Window", kind + "_window"}

	for _, key := range keys {
		if window, ok := data[key].(map[string]interface{}); ok {
			pct := opencodeExtractPercent(window)
			reset := opencodeExtractResetSec(window)
			return pct, reset
		}
	}

	// Check nested: data.usage.rolling / data.usage.weekly etc
	if usage, ok := data["usage"].(map[string]interface{}); ok {
		for _, key := range keys {
			if window, ok := usage[key].(map[string]interface{}); ok {
				pct := opencodeExtractPercent(window)
				reset := opencodeExtractResetSec(window)
				return pct, reset
			}
		}
	}

	// Deep search
	pct, reset := opencodeDeepSearch(data, kind, 0)
	return pct, reset
}

func opencodeDeepSearch(data map[string]interface{}, kind string, depth int) (float64, int) {
	if depth > 3 {
		return 0, 0
	}
	for key, val := range data {
		lower := strings.ToLower(key)
		if (kind == "rolling" && (strings.Contains(lower, "rolling") || strings.Contains(lower, "hour") || strings.Contains(lower, "5h"))) ||
			(kind == "weekly" && (strings.Contains(lower, "weekly") || strings.Contains(lower, "week"))) {
			if sub, ok := val.(map[string]interface{}); ok {
				pct := opencodeExtractPercent(sub)
				reset := opencodeExtractResetSec(sub)
				if pct > 0 || reset > 0 {
					return pct, reset
				}
			}
		}
		if sub, ok := val.(map[string]interface{}); ok {
			pct, reset := opencodeDeepSearch(sub, kind, depth+1)
			if pct > 0 || reset > 0 {
				return pct, reset
			}
		}
	}
	return 0, 0
}

var opencodePercentKeys = []string{"usagePercent", "usedPercent", "percentUsed", "percent", "usage_percent", "used_percent", "utilization"}
var opencodeResetKeys = []string{"resetInSec", "resetInSeconds", "resetSeconds", "reset_sec", "reset_in_sec", "resetsInSec", "resetIn"}

func opencodeExtractPercent(window map[string]interface{}) float64 {
	for _, key := range opencodePercentKeys {
		if v, ok := getFloat(window[key]); ok {
			return opencodePercent(v)
		}
	}
	return 0
}

func opencodeExtractResetSec(window map[string]interface{}) int {
	for _, key := range opencodeResetKeys {
		if v, ok := getInt(window[key]); ok {
			return v
		}
	}
	return 0
}

func getFloat(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case string:
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f, true
		}
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return f, true
		}
	}
	return 0, false
}

func getInt(v interface{}) (int, bool) {
	switch val := v.(type) {
	case float64:
		return int(val), true
	case int:
		return val, true
	case int64:
		return int(val), true
	case string:
		if i, err := strconv.Atoi(val); err == nil {
			return i, true
		}
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

// --- OpenCode Go Provider ---

type openCodeGoProvider struct{}

func (p *openCodeGoProvider) ID() string          { return "opencode-go" }
func (p *openCodeGoProvider) DisplayName() string { return "OpenCode Go" }

func (p *openCodeGoProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	cookie := strings.TrimSpace(auth.CookieHeader)
	if cookie == "" {
		return ProviderStatus{}, fmt.Errorf("no session cookie")
	}

	workspaceID, err := opencodeFetchWorkspaceID(ctx, cookie)
	if err != nil {
		return ProviderStatus{
			ProviderID:  "opencode-go",
			DisplayName: "OpenCode Go",
			Health:      "auth_required",
			Severity:    "info",
			Details:     "Session expired or invalid. Re-import from browser.",
			WindowLabel: "Usage",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	pageURL := fmt.Sprintf("%s/workspace/%s/go", opencodeBaseURL, workspaceID)
	text, err := opencodeFetchPage(ctx, pageURL, cookie, "OpenCode Go")
	if err != nil {
		if strings.Contains(err.Error(), "session expired") {
			return ProviderStatus{
				ProviderID:  "opencode-go",
				DisplayName: "OpenCode Go",
				Health:      "auth_required",
				Severity:    "info",
				Details:     "Session expired or invalid. Re-import from browser.",
				WindowLabel: "Usage",
				Timestamp:   time.Now().UTC(),
			}, nil
		}
		return ProviderStatus{}, err
	}

	var data map[string]interface{}
	var rollingPct, weeklyPct, monthlyPct float64
	var rollingResetSec, weeklyResetSec, monthlyResetSec int

	if json.Unmarshal([]byte(text), &data) == nil {
		rollingPct, rollingResetSec = opencodeExtractWindow(data, "rolling")
		weeklyPct, weeklyResetSec = opencodeExtractWindow(data, "weekly")
		monthlyPct, monthlyResetSec = opencodeExtractWindow(data, "monthly")
	}

	if rollingPct == 0 && weeklyPct == 0 {
		rollingPct, _ = opencodeExtractFloat(text, `rollingUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
		rollingResetSec, _ = opencodeExtractInt(text, `rollingUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
		weeklyPct, _ = opencodeExtractFloat(text, `weeklyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
		weeklyResetSec, _ = opencodeExtractInt(text, `weeklyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
		monthlyPct, _ = opencodeExtractFloat(text, `monthlyUsage[^}]*?usagePercent\s*:\s*([0-9]+(?:\.[0-9]+)?)`)
		monthlyResetSec, _ = opencodeExtractInt(text, `monthlyUsage[^}]*?resetInSec\s*:\s*([0-9]+)`)
	}

	rollingPct = opencodePercent(rollingPct)
	weeklyPct = opencodePercent(weeklyPct)
	monthlyPct = opencodePercent(monthlyPct)

	usagePct := rollingPct
	if weeklyPct > usagePct {
		usagePct = weeklyPct
	}
	if monthlyPct > usagePct {
		usagePct = monthlyPct
	}
	resetSec := rollingResetSec
	if weeklyResetSec > 0 && (weeklyResetSec < resetSec || resetSec == 0) {
		resetSec = weeklyResetSec
	}
	if monthlyResetSec > 0 && (monthlyResetSec < resetSec || resetSec == 0) {
		resetSec = monthlyResetSec
	}
	resetAt := time.Now().Add(time.Duration(resetSec) * time.Second)

	severity := severityForPercent(usagePct)

	var details string
	if resetSec > 0 {
		details = fmt.Sprintf("R %.0f%% / W %.0f%% / M %.0f%%, resets in %s", rollingPct, weeklyPct, monthlyPct, durationUntil(resetAt))
	} else {
		details = fmt.Sprintf("Rolling %.0f%% / Weekly %.0f%% / Monthly %.0f%%", rollingPct, weeklyPct, monthlyPct)
	}

	return ProviderStatus{
		ProviderID:     "opencode-go",
		DisplayName:    "OpenCode Go",
		UsagePercent:   usagePct,
		RemainingLabel: fmt.Sprintf("R%.0f%% W%.0f%% M%.0f%%", rollingPct, weeklyPct, monthlyPct),
		WindowLabel:    "Rolling + Weekly + Monthly",
		ResetAt:        resetAt,
		Severity:       severity,
		Health:         "ready",
		DeepLink:       fmt.Sprintf("https://opencode.ai/workspace/%s/go", workspaceID),
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- GitHub Copilot Provider ---

type copilotProvider struct{}

func (p *copilotProvider) ID() string          { return "copilot" }
func (p *copilotProvider) DisplayName() string { return "Copilot" }

type copilotQuotaSnapshot struct {
	Entitlement      float64 `json:"entitlement"`
	Remaining        float64 `json:"remaining"`
	PercentRemaining float64 `json:"percent_remaining"`
	QuotaID          string  `json:"quota_id"`
	Unlimited        bool    `json:"unlimited"`
}

type copilotQuotaSnapshots struct {
	PremiumInteractions *copilotQuotaSnapshot `json:"premium_interactions"`
	Chat                *copilotQuotaSnapshot `json:"chat"`
}

type copilotUsageResponse struct {
	QuotaSnapshots    copilotQuotaSnapshots `json:"quota_snapshots"`
	CopilotPlan       string                `json:"copilot_plan"`
	TokenBasedBilling bool                  `json:"token_based_billing"`
	QuotaResetDate    string                `json:"quota_reset_date"`
}

func copilotNormalizedHost(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "github.com"
	}
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
	}
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.Trim(host, "/")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.Trim(host, ".")
	if host == "" {
		return "github.com"
	}
	return strings.ToLower(host)
}

func copilotAPIHost(enterpriseHost string) string {
	host := copilotNormalizedHost(enterpriseHost)
	if host == "github.com" {
		return "api.github.com"
	}
	if strings.HasPrefix(host, "api.") {
		return host
	}
	return "api." + host
}

func copilotUsageURL(enterpriseHost string) string {
	return "https://" + copilotAPIHost(enterpriseHost) + "/copilot_internal/user"
}

func (q *copilotQuotaSnapshot) usedPercent() float64 {
	if q == nil {
		return 0
	}
	if q.Unlimited {
		return 0
	}
	return max(0, 100-q.PercentRemaining)
}

func (q *copilotQuotaSnapshot) hasUsablePercent() bool {
	if q == nil || q.isPlaceholder() {
		return false
	}
	if q.Unlimited {
		return true
	}
	return q.PercentRemaining > 0 || q.Entitlement > 0
}

func (q *copilotQuotaSnapshot) isPlaceholder() bool {
	if q == nil {
		return true
	}
	if q.Unlimited {
		return false
	}
	if q.Entitlement == 0 && q.Remaining == 0 && q.PercentRemaining == 0 {
		return true
	}
	return q.Entitlement == 0 && q.Remaining == 0
}

func parseCopilotResetDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func setCopilotHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Editor-Version", "vscode/1.96.2")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("X-Github-Api-Version", "2025-04-01")
}

func (p *copilotProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	token := strings.TrimSpace(authToken(auth))
	if token == "" {
		return ProviderStatus{
			ProviderID:  "copilot",
			DisplayName: "Copilot",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "GitHub Copilot",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	usageURL := copilotUsageURL(copilotEnterpriseHost)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return ProviderStatus{}, err
	}
	setCopilotHeaders(req, token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return ProviderStatus{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ProviderStatus{}, err
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return ProviderStatus{
			ProviderID:  "copilot",
			DisplayName: "Copilot",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "GitHub Copilot",
			Details:     "Token expired or invalid",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	if resp.StatusCode >= 400 {
		return ProviderStatus{}, formatHTTPError("Copilot", resp.StatusCode, body)
	}

	var usage copilotUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		return ProviderStatus{}, fmt.Errorf("Copilot parse error: %w", err)
	}

	planLabel := strings.TrimSpace(usage.CopilotPlan)
	if planLabel == "" {
		planLabel = "Unknown"
	} else if len(planLabel) > 1 {
		planLabel = strings.ToUpper(planLabel[:1]) + strings.ToLower(planLabel[1:])
	} else {
		planLabel = strings.ToUpper(planLabel)
	}
	resetAt := parseCopilotResetDate(usage.QuotaResetDate)

	premium := usage.QuotaSnapshots.PremiumInteractions
	chat := usage.QuotaSnapshots.Chat

	var primary *copilotQuotaSnapshot
	var secondary *copilotQuotaSnapshot
	if premium != nil && premium.hasUsablePercent() {
		primary = premium
		secondary = chat
	} else if chat != nil && chat.hasUsablePercent() {
		secondary = chat
	} else if usage.TokenBasedBilling {
		return ProviderStatus{
			ProviderID:   "copilot",
			DisplayName:  "Copilot",
			WindowLabel:  fmt.Sprintf("%s plan", planLabel),
			Health:       "ready",
			Severity:     "info",
			SummaryValue: "Active",
			Details:      "Token-based billing (no quota meter)",
			DeepLink:     "https://github.com/settings/copilot",
			Timestamp:    time.Now().UTC(),
		}, nil
	} else {
		return ProviderStatus{
			ProviderID:  "copilot",
			DisplayName: "Copilot",
			Health:      "degraded",
			Severity:    "info",
			WindowLabel: fmt.Sprintf("%s plan", planLabel),
			Details:     "Usage quota data unavailable",
			DeepLink:    "https://github.com/settings/copilot",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	usagePct := 0.0
	windowLabel := fmt.Sprintf("%s plan", planLabel)
	var detailsParts []string

	if primary != nil {
		usagePct = primary.usedPercent()
		windowLabel = fmt.Sprintf("%s · Premium", planLabel)
		detailsParts = append(detailsParts, fmt.Sprintf("Premium %.0f%%", usagePct))
	}
	if secondary != nil && secondary.hasUsablePercent() {
		chatPct := secondary.usedPercent()
		if chatPct > usagePct {
			usagePct = chatPct
		}
		detailsParts = append(detailsParts, fmt.Sprintf("Chat %.0f%%", chatPct))
		if primary == nil {
			windowLabel = fmt.Sprintf("%s · Chat", planLabel)
		}
	}

	details := strings.Join(detailsParts, ", ")
	if !resetAt.IsZero() {
		details = fmt.Sprintf("%s, resets in %s", details, durationUntil(resetAt))
	}

	return ProviderStatus{
		ProviderID:     "copilot",
		DisplayName:    "Copilot",
		UsagePercent:   usagePct,
		RemainingLabel: details,
		WindowLabel:    windowLabel,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://github.com/settings/copilot",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Cursor Provider ---

type cursorProvider struct{}

func (p *cursorProvider) ID() string          { return "cursor" }
func (p *cursorProvider) DisplayName() string { return "Cursor" }

type cursorPlanUsage struct {
	Enabled         *bool    `json:"enabled"`
	Used            *int     `json:"used"`
	Limit           *int     `json:"limit"`
	Remaining       *int     `json:"remaining"`
	AutoPercentUsed *float64 `json:"autoPercentUsed"`
	APIPercentUsed  *float64 `json:"apiPercentUsed"`
	TotalPercentUsed *float64 `json:"totalPercentUsed"`
}

type cursorOnDemandUsage struct {
	Enabled   *bool `json:"enabled"`
	Used      *int  `json:"used"`
	Limit     *int  `json:"limit"`
	Remaining *int  `json:"remaining"`
}

type cursorOverallUsage struct {
	Enabled   *bool `json:"enabled"`
	Used      *int  `json:"used"`
	Limit     *int  `json:"limit"`
	Remaining *int  `json:"remaining"`
}

type cursorIndividualUsage struct {
	Plan     *cursorPlanUsage     `json:"plan"`
	OnDemand *cursorOnDemandUsage `json:"onDemand"`
	Overall  *cursorOverallUsage  `json:"overall"`
}

type cursorPooledUsage struct {
	Enabled   *bool `json:"enabled"`
	Used      *int  `json:"used"`
	Limit     *int  `json:"limit"`
	Remaining *int  `json:"remaining"`
}

type cursorTeamUsage struct {
	OnDemand *cursorOnDemandUsage `json:"onDemand"`
	Pooled   *cursorPooledUsage   `json:"pooled"`
}

type cursorUsageSummary struct {
	BillingCycleStart *string                `json:"billingCycleStart"`
	BillingCycleEnd   *string                `json:"billingCycleEnd"`
	MembershipType    *string                `json:"membershipType"`
	IndividualUsage   *cursorIndividualUsage `json:"individualUsage"`
	TeamUsage         *cursorTeamUsage       `json:"teamUsage"`
}

type cursorUserInfo struct {
	Email *string `json:"email"`
	Name  *string `json:"name"`
	Sub   *string `json:"sub"`
}

type cursorLegacyUsage struct {
	GPT4 struct {
		NumRequests      *int `json:"numRequests"`
		NumRequestsTotal *int `json:"numRequestsTotal"`
		MaxRequestUsage  *int `json:"maxRequestUsage"`
	} `json:"gpt-4"`
}

func cursorNormalizePercent(v *float64) float64 {
	if v == nil {
		return 0
	}
	pct := *v
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func parseCursorBillingDate(raw *string) time.Time {
	if raw == nil {
		return time.Time{}
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		return time.Time{}
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func cursorPlanPercent(summary cursorUsageSummary, requestUsage *cursorLegacyUsage) float64 {
	if requestUsage != nil {
		limit := requestUsage.GPT4.MaxRequestUsage
		used := requestUsage.GPT4.NumRequestsTotal
		if used == nil {
			used = requestUsage.GPT4.NumRequests
		}
		if limit != nil && *limit > 0 && used != nil {
			return min(100, (float64(*used)/float64(*limit))*100)
		}
	}

	plan := summary.IndividualUsage
	if plan == nil {
		return 0
	}

	if plan.Plan != nil {
		if plan.Plan.TotalPercentUsed != nil {
			return cursorNormalizePercent(plan.Plan.TotalPercentUsed)
		}
		auto := cursorNormalizePercent(plan.Plan.AutoPercentUsed)
		api := cursorNormalizePercent(plan.Plan.APIPercentUsed)
		if auto > 0 && api > 0 {
			return (auto + api) / 2
		}
		if api > 0 {
			return api
		}
		if auto > 0 {
			return auto
		}
		if plan.Plan.Limit != nil && *plan.Plan.Limit > 0 && plan.Plan.Used != nil {
			return min(100, (float64(*plan.Plan.Used)/float64(*plan.Plan.Limit))*100)
		}
	}

	if plan.Overall != nil && plan.Overall.Limit != nil && *plan.Overall.Limit > 0 && plan.Overall.Used != nil {
		return min(100, (float64(*plan.Overall.Used)/float64(*plan.Overall.Limit))*100)
	}

	if summary.TeamUsage != nil && summary.TeamUsage.Pooled != nil &&
		summary.TeamUsage.Pooled.Limit != nil && *summary.TeamUsage.Pooled.Limit > 0 &&
		summary.TeamUsage.Pooled.Used != nil {
		return min(100, (float64(*summary.TeamUsage.Pooled.Used)/float64(*summary.TeamUsage.Pooled.Limit))*100)
	}

	return 0
}

func cursorMembershipLabel(summary cursorUsageSummary) string {
	if summary.MembershipType != nil {
		label := strings.TrimSpace(*summary.MembershipType)
		if label != "" {
			return label
		}
	}
	return "Usage"
}

func (p *cursorProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	cookie := strings.TrimSpace(auth.CookieHeader)
	if cookie == "" {
		return ProviderStatus{
			ProviderID:  "cursor",
			DisplayName: "Cursor",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Cursor",
			Details:     "Import a browser session from cursor.com",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	summary, err := cursorFetchUsageSummary(ctx, cookie)
	if err != nil {
		if strings.Contains(err.Error(), "session expired") || strings.Contains(err.Error(), "not logged in") {
			return ProviderStatus{
				ProviderID:  "cursor",
				DisplayName: "Cursor",
				Health:      "auth_required",
				Severity:    "info",
				WindowLabel: "Cursor",
				Details:     "Session expired or invalid. Re-import from browser.",
				Timestamp:   time.Now().UTC(),
			}, nil
		}
		return ProviderStatus{}, err
	}

	userInfo, _ := cursorFetchUserInfo(ctx, cookie)
	var requestUsage *cursorLegacyUsage
	if userInfo != nil && userInfo.Sub != nil && strings.TrimSpace(*userInfo.Sub) != "" {
		requestUsage, _ = cursorFetchRequestUsage(ctx, cookie, strings.TrimSpace(*userInfo.Sub))
	}

	usagePct := cursorPlanPercent(summary, requestUsage)
	resetAt := parseCursorBillingDate(summary.BillingCycleEnd)
	windowLabel := cursorMembershipLabel(summary)

	autoPct := 0.0
	apiPct := 0.0
	if summary.IndividualUsage != nil && summary.IndividualUsage.Plan != nil {
		autoPct = cursorNormalizePercent(summary.IndividualUsage.Plan.AutoPercentUsed)
		apiPct = cursorNormalizePercent(summary.IndividualUsage.Plan.APIPercentUsed)
	}

	detailsParts := []string{fmt.Sprintf("Total %.0f%%", usagePct)}
	if autoPct > 0 {
		detailsParts = append(detailsParts, fmt.Sprintf("Auto %.0f%%", autoPct))
	}
	if apiPct > 0 {
		detailsParts = append(detailsParts, fmt.Sprintf("API %.0f%%", apiPct))
	}
	if !resetAt.IsZero() {
		detailsParts = append(detailsParts, fmt.Sprintf("resets in %s", durationUntil(resetAt)))
	}

	accountDetail := ""
	if userInfo != nil && userInfo.Email != nil && strings.TrimSpace(*userInfo.Email) != "" {
		accountDetail = strings.TrimSpace(*userInfo.Email)
	}

	details := strings.Join(detailsParts, ", ")
	if accountDetail != "" {
		details = accountDetail + " · " + details
	}

	return ProviderStatus{
		ProviderID:     "cursor",
		DisplayName:    "Cursor",
		UsagePercent:   usagePct,
		RemainingLabel: fmt.Sprintf("%.0f%% used", usagePct),
		WindowLabel:    windowLabel,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://cursor.com/dashboard",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

func cursorFetchUsageSummary(ctx context.Context, cookieHeader string) (cursorUsageSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cursor.com/api/usage-summary", nil)
	if err != nil {
		return cursorUsageSummary{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return cursorUsageSummary{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cursorUsageSummary{}, err
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return cursorUsageSummary{}, fmt.Errorf("session expired or not logged in")
	}
	if resp.StatusCode >= 400 {
		return cursorUsageSummary{}, formatHTTPError("Cursor", resp.StatusCode, body)
	}

	var summary cursorUsageSummary
	if err := json.Unmarshal(body, &summary); err != nil {
		return cursorUsageSummary{}, fmt.Errorf("Cursor parse error: %w", err)
	}
	return summary, nil
}

func cursorFetchUserInfo(ctx context.Context, cookieHeader string) (*cursorUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cursor.com/api/auth/me", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Cursor user info error (%d)", resp.StatusCode)
	}

	var info cursorUserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func cursorFetchRequestUsage(ctx context.Context, cookieHeader, userID string) (*cursorLegacyUsage, error) {
	reqURL := "https://cursor.com/api/usage?user=" + url.QueryEscape(userID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Cursor request usage error (%d)", resp.StatusCode)
	}

	var usage cursorLegacyUsage
	if err := json.Unmarshal(body, &usage); err != nil {
		return nil, err
	}
	return &usage, nil
}

// --- CommandCode Provider ---

type commandCodeProvider struct{}

func (p *commandCodeProvider) ID() string          { return "commandcode" }
func (p *commandCodeProvider) DisplayName() string { return "Command Code" }

type commandCodePlan struct {
	ID                string
	DisplayName       string
	MonthlyCreditsUSD float64
}

var commandCodePlans = map[string]commandCodePlan{
	"individual-go":    {ID: "individual-go", DisplayName: "Go", MonthlyCreditsUSD: 10},
	"individual-pro":   {ID: "individual-pro", DisplayName: "Pro", MonthlyCreditsUSD: 30},
	"individual-max":   {ID: "individual-max", DisplayName: "Max", MonthlyCreditsUSD: 150},
	"individual-ultra": {ID: "individual-ultra", DisplayName: "Ultra", MonthlyCreditsUSD: 300},
}

var commandCodeSessionCookieNames = []string{
	"__Host-better-auth.session_token",
	"__Secure-better-auth.session_token",
	"better-auth.session_token",
}

type commandCodeCreditsPayload struct {
	MonthlyCredits           float64
	PurchasedCredits         float64
	PremiumMonthlyCredits    float64
	OpensourceMonthlyCredits float64
}

type commandCodeSubscriptionPayload struct {
	PlanID           string
	Status           string
	CurrentPeriodEnd time.Time
}

func commandCodePlanForID(planID string) (commandCodePlan, bool) {
	plan, ok := commandCodePlans[strings.ToLower(strings.TrimSpace(planID))]
	return plan, ok
}

func commandCodeSessionCookie(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}

	if !strings.Contains(raw, "=") && !strings.Contains(raw, ";") {
		return "__Secure-better-auth.session_token=" + raw, true
	}

	byName := make(map[string]string)
	for _, chunk := range strings.Split(raw, ";") {
		trimmed := strings.TrimSpace(chunk)
		if trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if name == "" || value == "" {
			continue
		}
		byName[strings.ToLower(name)] = name + "=" + value
	}

	for _, expected := range commandCodeSessionCookieNames {
		if cookie, ok := byName[strings.ToLower(expected)]; ok {
			return cookie, true
		}
	}
	return "", false
}

func commandCodeDoubleValue(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		if val == val {
			return val, true
		}
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case json.Number:
		if f, err := val.Float64(); err == nil {
			return f, true
		}
	case string:
		trimmed := strings.TrimSpace(val)
		if trimmed == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func commandCodeParseDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func commandCodeParseCredits(body []byte) (commandCodeCreditsPayload, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return commandCodeCreditsPayload{}, fmt.Errorf("Command Code credits parse error: %w", err)
	}
	credits, ok := root["credits"].(map[string]interface{})
	if !ok {
		return commandCodeCreditsPayload{}, fmt.Errorf("Command Code credits missing credits object")
	}
	monthly, ok := commandCodeDoubleValue(credits["monthlyCredits"])
	if !ok {
		return commandCodeCreditsPayload{}, fmt.Errorf("Command Code credits missing monthlyCredits")
	}
	purchased, _ := commandCodeDoubleValue(credits["purchasedCredits"])
	premium, _ := commandCodeDoubleValue(credits["premiumMonthlyCredits"])
	opensource, _ := commandCodeDoubleValue(credits["opensourceMonthlyCredits"])
	return commandCodeCreditsPayload{
		MonthlyCredits:           monthly,
		PurchasedCredits:         purchased,
		PremiumMonthlyCredits:    premium,
		OpensourceMonthlyCredits: opensource,
	}, nil
}

func commandCodeParseSubscription(body []byte) (*commandCodeSubscriptionPayload, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("Command Code subscription parse error: %w", err)
	}
	success, ok := root["success"].(bool)
	if !ok {
		return nil, fmt.Errorf("Command Code subscription missing success flag")
	}
	if !success {
		return nil, fmt.Errorf("Command Code subscription unsuccessful response")
	}
	dataValue, ok := root["data"]
	if !ok {
		return nil, fmt.Errorf("Command Code subscription missing data")
	}
	if dataValue == nil {
		return nil, nil
	}
	data, ok := dataValue.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Command Code subscription invalid data")
	}
	planID, _ := data["planId"].(string)
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil, fmt.Errorf("Command Code subscription missing planId")
	}
	status, _ := data["status"].(string)
	if strings.TrimSpace(status) == "" {
		status = "unknown"
	}
	var periodEnd time.Time
	if raw, ok := data["currentPeriodEnd"].(string); ok {
		periodEnd = commandCodeParseDate(raw)
	}
	return &commandCodeSubscriptionPayload{
		PlanID:           planID,
		Status:           status,
		CurrentPeriodEnd: periodEnd,
	}, nil
}

func commandCodeSetRequestHeaders(req *http.Request, cookieHeader string) {
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36")
	req.Header.Set("Origin", "https://commandcode.ai")
	req.Header.Set("Referer", "https://commandcode.ai/")
}

func commandCodeFetchEndpoint(ctx context.Context, path string, cookieHeader string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.commandcode.ai"+path, nil)
	if err != nil {
		return nil, 0, err
	}
	commandCodeSetRequestHeaders(req, cookieHeader)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func commandCodeUsagePercent(remaining, total float64) float64 {
	if total <= 0 {
		return 0
	}
	used := total - remaining
	if used < 0 {
		used = 0
	}
	if used > total {
		used = total
	}
	return min(100, (used/total)*100)
}

func (p *commandCodeProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	cookieHeader, ok := commandCodeSessionCookie(auth.CookieHeader)
	if !ok {
		return ProviderStatus{
			ProviderID:  "commandcode",
			DisplayName: "Command Code",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Command Code",
			Details:     "Import a browser session from commandcode.ai",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	creditsBody, creditsStatus, err := commandCodeFetchEndpoint(ctx, "/internal/billing/credits", cookieHeader)
	if err != nil {
		return ProviderStatus{}, err
	}
	if creditsStatus == 401 || creditsStatus == 403 {
		return ProviderStatus{
			ProviderID:  "commandcode",
			DisplayName: "Command Code",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Command Code",
			Details:     "Session expired or invalid. Re-import from browser.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	if creditsStatus >= 400 {
		return ProviderStatus{}, formatHTTPError("Command Code", creditsStatus, creditsBody)
	}

	credits, err := commandCodeParseCredits(creditsBody)
	if err != nil {
		return ProviderStatus{}, err
	}

	type subscriptionResult struct {
		payload *commandCodeSubscriptionPayload
		err     error
	}
	subCh := make(chan subscriptionResult, 1)
	go func() {
		body, status, fetchErr := commandCodeFetchEndpoint(ctx, "/internal/billing/subscriptions", cookieHeader)
		if fetchErr != nil {
			subCh <- subscriptionResult{err: fetchErr}
			return
		}
		if status == 401 || status == 403 {
			subCh <- subscriptionResult{err: fmt.Errorf("session expired or invalid")}
			return
		}
		if status >= 400 {
			subCh <- subscriptionResult{err: formatHTTPError("Command Code", status, body)}
			return
		}
		payload, parseErr := commandCodeParseSubscription(body)
		subCh <- subscriptionResult{payload: payload, err: parseErr}
	}()

	var subscription *commandCodeSubscriptionPayload
	select {
	case result := <-subCh:
		if result.err == nil {
			subscription = result.payload
		}
	case <-time.After(2 * time.Second):
	}

	planLabel := "Free"
	var plan commandCodePlan
	hasPlan := false
	if subscription != nil && strings.EqualFold(subscription.Status, "active") {
		if resolved, ok := commandCodePlanForID(subscription.PlanID); ok {
			plan = resolved
			hasPlan = true
			planLabel = plan.DisplayName
		} else {
			return ProviderStatus{}, fmt.Errorf("Command Code unknown plan: %s", subscription.PlanID)
		}
	}

	usagePct := 0.0
	summaryValue := fmt.Sprintf("$%.2f left", credits.MonthlyCredits)
	windowLabel := planLabel + " monthly credits"
	var detailsParts []string
	var resetAt time.Time
	if subscription != nil {
		resetAt = subscription.CurrentPeriodEnd
	}

	if hasPlan {
		usagePct = commandCodeUsagePercent(credits.MonthlyCredits, plan.MonthlyCreditsUSD)
		used := max(0, min(plan.MonthlyCreditsUSD, plan.MonthlyCreditsUSD-credits.MonthlyCredits))
		summaryValue = fmt.Sprintf("%.0f%%", usagePct)
		detailsParts = []string{
			fmt.Sprintf("$%.2f of $%.0f used", used, plan.MonthlyCreditsUSD),
			fmt.Sprintf("$%.2f monthly remaining", credits.MonthlyCredits),
		}
	} else {
		detailsParts = []string{fmt.Sprintf("$%.2f monthly remaining", credits.MonthlyCredits)}
	}

	if credits.PurchasedCredits > 0 {
		detailsParts = append(detailsParts, fmt.Sprintf("$%.2f purchased", credits.PurchasedCredits))
	}
	if !resetAt.IsZero() {
		detailsParts = append(detailsParts, fmt.Sprintf("resets in %s", durationUntil(resetAt)))
	}

	if !hasPlan && credits.MonthlyCredits <= 0 && credits.PurchasedCredits <= 0 {
		return ProviderStatus{
			ProviderID:  "commandcode",
			DisplayName: "Command Code",
			Health:      "degraded",
			Severity:    "info",
			WindowLabel: "Free tier",
			Details:     "No monthly grant or purchased credits",
			DeepLink:    "https://commandcode.ai/studio",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	return ProviderStatus{
		ProviderID:     "commandcode",
		DisplayName:    "Command Code",
		UsagePercent:   usagePct,
		RemainingLabel: fmt.Sprintf("$%.2f / plan", credits.MonthlyCredits),
		WindowLabel:    windowLabel,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://commandcode.ai/studio",
		SummaryValue:   summaryValue,
		Details:        strings.Join(detailsParts, ", "),
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Provider Registry ---

func getAllProviders() []Provider {
	return []Provider{
		&openRouterProvider{},
		&claudeProvider{},
		&codexProvider{},
		&openCodeProvider{},
		&openCodeGoProvider{},
		&copilotProvider{},
		&cursorProvider{},
		&commandCodeProvider{},
		&geminiProvider{},
		&windsurfProvider{},
		&grokProvider{},
		&augmentProvider{},
		&factoryProvider{},
		&zedProvider{},
		&warpProvider{},
		&devinProvider{},
		&kiroProvider{},
	}
}

// --- Severity helpers ---

func severityForPercent(pct float64) string {
	if pct >= 90 {
		return "critical"
	}
	if pct >= 75 {
		return "warning"
	}
	return "info"
}

func durationUntil(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return "now"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 24 {
		days := h / 24
		return fmt.Sprintf("%dd %dh", days, h%24)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func formatHTTPError(provider string, statusCode int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("%s API error (%d)", provider, statusCode)
	}

	if strings.Contains(strings.ToLower(message), "<html") {
		message = extractHTMLTitle(message)
	}

	message = strings.Join(strings.Fields(message), " ")
	message = truncateMessage(message, 160)
	if message == "" {
		return fmt.Errorf("%s API error (%d)", provider, statusCode)
	}

	return fmt.Errorf("%s API error (%d): %s", provider, statusCode, message)
}

func extractHTMLTitle(body string) string {
	lower := strings.ToLower(body)
	start := strings.Index(lower, "<title>")
	end := strings.Index(lower, "</title>")
	if start >= 0 && end > start {
		title := body[start+len("<title>") : end]
		title = html.UnescapeString(strings.TrimSpace(title))
		if title != "" {
			return title
		}
	}

	return "Unexpected HTML error response"
}

func truncateMessage(message string, limit int) string {
	if len(message) <= limit {
		return message
	}
	if limit <= 3 {
		return message[:limit]
	}
	return message[:limit-3] + "..."
}

func sanitizeDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "Request failed"
	}
	if strings.Contains(strings.ToLower(detail), "<html") {
		detail = extractHTMLTitle(detail)
	}
	detail = strings.Join(strings.Fields(detail), " ")
	return truncateMessage(detail, 160)
}

func nextSaturday() time.Time {
	now := time.Now()
	daysUntilSat := (6 - int(now.Weekday()) + 7) % 7
	if daysUntilSat == 0 {
		daysUntilSat = 7
	}
	sat := now.AddDate(0, 0, daysUntilSat)
	return time.Date(sat.Year(), sat.Month(), sat.Day(), 0, 0, 0, 0, sat.Location())
}

func endOfMonth() time.Time {
	now := time.Now()
	firstOfNext := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
	return firstOfNext.Add(-time.Second)
}

// --- Auth resolution ---

func (p *aiProviderPlugin) authsForProvider(providerID string) []AuthContext {
	if auths, ok := p.providerAuth[providerID]; ok {
		return auths
	}
	if providerID == "opencode-go" {
		return p.providerAuth["opencode"]
	}
	return nil
}

func (p *aiProviderPlugin) hasUsableAuth(auth AuthContext) bool {
	return authToken(auth) != "" || auth.CookieHeader != ""
}

func authToken(auth AuthContext) string {
	if t := strings.TrimSpace(auth.APIKey); t != "" {
		return t
	}
	return strings.TrimSpace(auth.AccessToken)
}

// --- Snapshot builder ---

func effectiveDisplayName(p ProviderStatus) string {
	if p.AccountDisplayName != "" {
		return p.DisplayName + " (" + p.AccountDisplayName + ")"
	}
	return p.DisplayName
}

type fetchResult struct {
	status ProviderStatus
	err    error
}

func (p *aiProviderPlugin) emitProviderEvents(results map[string]fetchResult) {
	now := time.Now().UTC().Format(time.RFC3339)
	warnThreshold := p.config.WarningThreshold
	critThreshold := p.config.CriticalThreshold

	for key, res := range results {
		if res.status.Health != "ready" || res.err != nil {
			continue
		}

		currentPct := res.status.UsagePercent
		prevPct, hadPrev := p.prevUsage[key]
		displayName := effectiveDisplayName(res.status)

		if hadPrev {
			if prevPct < critThreshold && currentPct >= critThreshold {
				sendNotification("event", eventParams{
					Event: pluginEvent{
						Type:      "usage.threshold_crossed",
						PluginID:  pluginID,
						Message:   fmt.Sprintf("%s usage crossed critical threshold (%.0f%%)", displayName, currentPct),
						Severity:  "critical",
						Data:      map[string]string{"providerId": res.status.ProviderID, "accountId": res.status.AccountID, "usagePercent": fmt.Sprintf("%.0f", currentPct), "threshold": "critical"},
						Timestamp: now,
					},
				})
			} else if prevPct < warnThreshold && currentPct >= warnThreshold && currentPct < critThreshold {
				sendNotification("event", eventParams{
					Event: pluginEvent{
						Type:      "usage.threshold_crossed",
						PluginID:  pluginID,
						Message:   fmt.Sprintf("%s usage crossed warning threshold (%.0f%%)", displayName, currentPct),
						Severity:  "warning",
						Data:      map[string]string{"providerId": res.status.ProviderID, "accountId": res.status.AccountID, "usagePercent": fmt.Sprintf("%.0f", currentPct), "threshold": "warning"},
						Timestamp: now,
					},
				})
			}

			if prevPct-currentPct >= 10 {
				sendNotification("event", eventParams{
					Event: pluginEvent{
						Type:      "usage.decreased",
						PluginID:  pluginID,
						Message:   fmt.Sprintf("%s usage dropped from %.0f%% to %.0f%%", displayName, prevPct, currentPct),
						Severity:  "info",
						Data:      map[string]string{"providerId": res.status.ProviderID, "accountId": res.status.AccountID, "previous": fmt.Sprintf("%.0f", prevPct), "current": fmt.Sprintf("%.0f", currentPct)},
						Timestamp: now,
					},
				})
			}

			if currentPct-prevPct >= 10 && currentPct < warnThreshold {
				sendNotification("event", eventParams{
					Event: pluginEvent{
						Type:      "usage.increased",
						PluginID:  pluginID,
						Message:   fmt.Sprintf("%s usage rose from %.0f%% to %.0f%%", displayName, prevPct, currentPct),
						Severity:  "info",
						Data:      map[string]string{"providerId": res.status.ProviderID, "accountId": res.status.AccountID, "previous": fmt.Sprintf("%.0f", prevPct), "current": fmt.Sprintf("%.0f", currentPct)},
						Timestamp: now,
					},
				})
			}
		}

		p.prevUsage[key] = currentPct
	}
}

func (p *aiProviderPlugin) buildSnapshot() pluginSnapshot {
	allProviders := getAllProviders()
	enabledIDs := p.config.EnabledProviders

	enabledSet := make(map[string]bool)
	for _, id := range enabledIDs {
		enabledSet[strings.TrimSpace(id)] = true
	}

	results := make(map[string]fetchResult)

	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Second)
	defer cancel()

	// Fetch all enabled providers concurrently (one job per provider+account pair)
	type providerJob struct {
		provider Provider
		auth     AuthContext
		hasAuth  bool
		key      string
	}
	jobs := make([]providerJob, 0)
	for _, provider := range allProviders {
		if !enabledSet[provider.ID()] {
			continue
		}
		auths := p.authsForProvider(provider.ID())
		if len(auths) == 0 {
			jobs = append(jobs, providerJob{
				provider,
				AuthContext{},
				!providerRequiresHostAuth(provider.ID()),
				provider.ID(),
			})
		} else {
			for i, auth := range auths {
				key := provider.ID()
				if auth.AccountID != "" {
					key = key + ":" + auth.AccountID
				} else {
					key = key + ":" + strconv.Itoa(i)
				}
				hasAuth := !providerRequiresHostAuth(provider.ID()) || p.hasUsableAuth(auth)
				jobs = append(jobs, providerJob{provider, auth, hasAuth, key})
			}
		}
	}

	resultCh := make(chan struct {
		id  string
		res fetchResult
	}, len(jobs))

	for _, job := range jobs {
		go func(j providerJob) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[ai-provider-plugin] panic in provider %s goroutine: %v\n", j.provider.ID(), r)
					resultCh <- struct {
						id  string
						res fetchResult
					}{j.key, fetchResult{
						status: ProviderStatus{
							ProviderID:  j.provider.ID(),
							DisplayName: j.provider.DisplayName(),
							Health:      "error",
							Severity:    "info",
							Details:     fmt.Sprintf("Internal error: %v", r),
						},
					}}
				}
			}()
			if !j.hasAuth {
				logDebug("provider %s no auth, skipping fetch", j.key)
				resultCh <- struct {
					id  string
					res fetchResult
				}{j.key, fetchResult{
					status: ProviderStatus{
						ProviderID:  j.provider.ID(),
						DisplayName: j.provider.DisplayName(),
						Health:      "auth_required",
						Severity:    "info",
					},
				}}
				return
			}

			logDebug("provider %s starting fetch", j.key)
			reqCtx, reqCancel := context.WithTimeout(ctx, 20*time.Second)
			defer reqCancel()

			type fetchOutcome struct {
				status ProviderStatus
				err    error
			}
			done := make(chan fetchOutcome, 1)
			go func() {
				s, e := j.provider.Fetch(reqCtx, j.auth)
				if e == nil {
					s.AccountID = j.auth.AccountID
					s.AccountDisplayName = j.auth.DisplayName
				}
				done <- fetchOutcome{s, e}
			}()

			var status ProviderStatus
			var err error
			select {
			case outcome := <-done:
				status, err = outcome.status, outcome.err
			case <-time.After(25 * time.Second):
				status = ProviderStatus{
					ProviderID:  j.provider.ID(),
					DisplayName: j.provider.DisplayName(),
					AccountID:   j.auth.AccountID,
					AccountDisplayName: j.auth.DisplayName,
					Health:      "error",
					Severity:    "info",
					Details:     "Request timed out",
				}
				err = fmt.Errorf("fetch timed out after 25s")
			}

			if err != nil {
				logDebug("provider %s fetch error: %v", j.key, err)
				resultCh <- struct {
					id  string
					res fetchResult
				}{j.key, fetchResult{
					status: ProviderStatus{
						ProviderID:  j.provider.ID(),
						DisplayName: j.provider.DisplayName(),
						AccountID:   j.auth.AccountID,
						AccountDisplayName: j.auth.DisplayName,
						Health:      "error",
						Severity:    "info",
						Details:     sanitizeDetail(err.Error()),
					},
					err: err,
				}}
				return
			}
			logDebug("provider %s fetch done health=%s", j.key, status.Health)
			resultCh <- struct {
				id  string
				res fetchResult
			}{j.key, fetchResult{status: status}}
		}(job)
	}

	logDebug("waiting for %d provider results", len(jobs))
	for range jobs {
		r := <-resultCh
		results[r.id] = r.res
	}
	logDebug("all provider results received")

	// Emit events for threshold crosses and provider state changes
	p.emitProviderEvents(results)

	// If no providers enabled at all
	if len(jobs) == 0 {
		return pluginSnapshot{
			PluginID: pluginID,
			State:    "degraded",
			Summary:  pluginSummary{Title: "AI Providers", Value: "No providers enabled", Trend: "steady", Severity: "info", IconHint: "brain.head.profile"},
			Items:    []pluginItem{},
			Actions: []pluginAction{
				{ID: "open_settings", Label: "Open Settings"},
			},
			Alerts: []pluginAlert{
				{ID: "ai-no-providers", Severity: "info", Message: "No AI providers are enabled in plugin settings."},
			},
			RefreshAfter: 300,
			Health:       "degraded",
		}
	}

	// Build items (cards) for all fetched provider+account pairs
	cards := make([]ProviderStatus, 0, len(results))
	allAuthMissing := true
	allErrored := true
	hasAnyLive := false

	for _, res := range results {
		if res.status.Health == "ready" {
			allAuthMissing = false
			allErrored = false
			hasAnyLive = true
			cards = append(cards, res.status)
		} else if res.status.Health == "auth_required" {
			allErrored = false
			cards = append(cards, res.status)
		} else {
			cards = append(cards, res.status)
			allAuthMissing = false
		}
	}

	// Select menubar winner
	winner := selectWinner(cards)

	// Build snapshot from winner
	var summary pluginSummary
	var health string
	var state string
	var alerts = make([]pluginAlert, 0)
	items := make([]pluginItem, 0, len(cards))

	if allAuthMissing && len(jobs) > 0 {
		summary = pluginSummary{
			Title: "AI Providers", Value: "AI auth", Trend: "steady", Severity: "info", IconHint: "brain.head.profile",
		}
		health = "auth_required"
		state = "degraded"
		alerts = append(alerts, pluginAlert{
			ID: "ai-auth-required", Severity: "warning", Message: "AI provider credentials are missing. Set up auth in Settings.",
		})
	} else if allErrored && len(jobs) > 0 {
		summary = pluginSummary{
			Title: "AI Providers", Value: "AI unavailable", Trend: "steady", Severity: "warning", IconHint: "brain.head.profile",
		}
		health = "error"
		state = "degraded"
		alerts = append(alerts, pluginAlert{
			ID: "ai-all-error", Severity: "warning", Message: "All AI provider fetches failed. Check network and credentials.",
		})
	} else if winner.HasData {
		summary = pluginSummary{
			Title:    effectiveDisplayName(winner.ProviderStatus),
			Value:    winner.SummaryValue,
			Trend:    "steady",
			Severity: winner.Severity,
			IconHint: "brain.head.profile",
		}
		health = "ok"
		state = "ready"
	} else if hasAnyLive {
		// We have some live providers but no clear winner with data
		summary = pluginSummary{
			Title: "AI Providers", Value: "Connected", Trend: "steady", Severity: "info", IconHint: "brain.head.profile",
		}
		health = "ok"
		state = "ready"
	} else {
		summary = pluginSummary{
			Title: "AI Providers", Value: "Loading", Trend: "steady", Severity: "info", IconHint: "brain.head.profile",
		}
		health = "degraded"
		state = "degraded"
	}

	// Build items
	for _, card := range cards {
		items = append(items, cardToItem(card))
	}

	// Add threshold-based alerts for live providers
	for _, card := range cards {
		if card.Health == "ready" {
			if card.Severity == "critical" {
				alerts = append(alerts, pluginAlert{
					ID:       "ai-" + card.ProviderID + "-critical",
					Severity: "critical",
					Message:  fmt.Sprintf("%s usage is at %.0f%% (critical threshold)", card.DisplayName, card.UsagePercent),
				})
			} else if card.Severity == "warning" {
				alerts = append(alerts, pluginAlert{
					ID:       "ai-" + card.ProviderID + "-warning",
					Severity: "warning",
					Message:  fmt.Sprintf("%s usage is at %.0f%% (warning threshold)", card.DisplayName, card.UsagePercent),
				})
			}
		}
	}

	snapshot := pluginSnapshot{
		PluginID: pluginID,
		State:    state,
		Summary:  summary,
		Items:    items,
		Actions: []pluginAction{
			{ID: "refresh", Label: "Refresh"},
			{ID: "open_settings", Label: "Open Settings"},
		},
		Alerts:       alerts,
		RefreshAfter: 300,
		Health:       health,
	}

	p.lastSnapshot = &snapshot
	return snapshot
}

type providerWithData struct {
	ProviderStatus
	HasData bool
}

func selectWinner(cards []ProviderStatus) providerWithData {
	// Filter to providers with usable normalized data
	live := make([]ProviderStatus, 0)
	for _, c := range cards {
		if c.Health == "ready" && c.UsagePercent >= 0 {
			live = append(live, c)
		}
	}

	if len(live) == 0 {
		return providerWithData{HasData: false}
	}

	// Sort by selection rules:
	// 1. Prefer critical severity
	// 2. Highest UsagePercent
	// 3. Earliest ResetAt
	// 4. Stable provider priority
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i], live[j]

		// Rule 1: Critical severity wins
		aCritical := a.Severity == "critical"
		bCritical := b.Severity == "critical"
		if aCritical != bCritical {
			return aCritical
		}

		// Rule 2: Highest usage percent
		if a.UsagePercent != b.UsagePercent {
			return a.UsagePercent > b.UsagePercent
		}

		// Rule 3: Earliest reset
		aZero := a.ResetAt.IsZero()
		bZero := b.ResetAt.IsZero()
		if !aZero && !bZero {
			return a.ResetAt.Before(b.ResetAt)
		}
		if aZero != bZero {
			return !aZero // non-zero wins
		}

		// Rule 4: Stable provider priority
		return providerPriority[a.ProviderID] < providerPriority[b.ProviderID]
	})

	return providerWithData{ProviderStatus: live[0], HasData: true}
}

func cardToItem(p ProviderStatus) pluginItem {
	severity := p.Severity
	if p.Health == "auth_required" {
		severity = "info"
	}
	if p.Health == "error" {
		severity = "warning"
	}

	var title string
	var subtitle string
	var detail string

	displayTitle := effectiveDisplayName(p)
	actionLabel := "Open " + displayTitle

	switch p.Health {
	case "ready":
		title = displayTitle
		subtitle = p.WindowLabel
		detail = p.Details
	case "auth_required":
		title = displayTitle
		subtitle = "Auth required"
		detail = "Set up credentials in Settings"
	case "error":
		title = displayTitle
		subtitle = "Error"
		detail = sanitizeDetail(p.Details)
	case "degraded":
		title = displayTitle
		subtitle = p.WindowLabel
		detail = sanitizeDetail(p.Details)
	default:
		title = displayTitle
		subtitle = "Unknown"
		detail = sanitizeDetail(p.Details)
	}

	itemID := "ai-provider-" + p.ProviderID
	if p.AccountID != "" {
		itemID = itemID + "-" + p.AccountID
	}

	return pluginItem{
		ID:        itemID,
		Title:     title,
		Subtitle:  subtitle,
		Detail:    detail,
		Severity:  severity,
		Timestamp: p.Timestamp.Format(time.RFC3339),
		DeepLink:  p.DeepLink,
		Actions:   []pluginAction{{ID: "open_" + p.ProviderID, Label: actionLabel}},
		Metadata: map[string]string{
			"providerId":   p.ProviderID,
			"displayName":  p.DisplayName,
			"usagePercent": fmt.Sprintf("%.2f", p.UsagePercent),
			"windowLabel":  p.WindowLabel,
			"severity":     p.Severity,
			"health":       p.Health,
		},
	}
}

// --- Main ---

func main() {
	fmt.Fprintln(os.Stderr, "[ai-provider-plugin] process started")
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[ai-provider-plugin] panic: %v\n", r)
		}
	}()

	pl := &aiProviderPlugin{
		client:       &http.Client{Timeout: 12 * time.Second},
		config:       defaultConfig(),
		providerAuth: make(map[string][]AuthContext),
		prevUsage:    make(map[string]float64),
	}
	logDebug("started")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	fmt.Fprintln(os.Stderr, "[ai-provider-plugin] waiting for stdin...")
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			logDebug("decode request failed: %v", err)
			continue
		}
		logDebug("parsed method=%s id=%d", req.Method, req.ID)

		switch req.Method {
		case "initialize":
			if err := pl.handleInitialize(req); err != nil {
				logDebug("initialize failed: %v", err)
				sendError(req.ID, -32000, err.Error(), false, "Check AI provider credentials in Settings")
			}
		case "getStatus", "refresh":
			logDebug("building snapshot")
			sendResult(req.ID, pl.buildSnapshot())
		case "shutdown":
			logDebug("shutdown")
			sendResult(req.ID, nil)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[ai-provider-plugin] scanner error: %v\n", err)
	}
	fmt.Fprintln(os.Stderr, "[ai-provider-plugin] scanner loop exited")
}

func (p *aiProviderPlugin) handleInitialize(req rpcRequest) error {
	var params initializeParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return fmt.Errorf("decode initialize: %w", err)
	}

	plugindebug.ConfigureFromInitializeConfig(params.Config)
	p.config = parseConfig(params.Config)

	p.providerAuth = make(map[string][]AuthContext)

	// Process new multi-provider auth (accumulate multiple accounts per provider)
	for _, pa := range params.ProviderAuths {
		ac := AuthContext{
			ProviderID:   pa.ProviderID,
			Kind:         pa.Kind,
			AccountID:    pa.AccountID,
			AccessToken:  pa.AccessToken,
			APIKey:       pa.APIKey,
			CookieHeader: pa.CookieHeader,
			ExpiresAt:    pa.ExpiresAt,
			DisplayName:  pa.DisplayName,
		}
		p.providerAuth[pa.ProviderID] = append(p.providerAuth[pa.ProviderID], ac)
		logDebug("auth provider=%s account=%s kind=%s hasToken=%t hasKey=%t hasCookie=%t",
			pa.ProviderID, pa.AccountID, pa.Kind, pa.AccessToken != "", pa.APIKey != "", pa.CookieHeader != "")
	}

	logDebug("initialize config enabledProviders=%v", p.config.EnabledProviders)
	sendResult(req.ID, initializedPayload{
		Type:            "initialized",
		ProtocolVersion: protocolVersion,
		PluginVersion:   pluginVersion,
		Health:          "ok",
	})
	return nil
}

func sendResult(id int, result interface{}) {
	logDebug("sending result id=%d", id)
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(resp); err != nil {
		logDebug("json encode error: %v", err)
		return
	}
	_ = os.Stdout.Sync()
}

func sendError(id int, code int, message string, retryable bool, suggestedAction string) {
	logDebug("sending error id=%d code=%d message=%s", id, code, message)
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    &pluginErrorData{Retryable: retryable, SuggestedAction: suggestedAction},
		},
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(resp); err != nil {
		logDebug("json encode error: %v", err)
		return
	}
	_ = os.Stdout.Sync()
}

func sendNotification(method string, params interface{}) {
	notif := struct {
		JSONRPC string      `json:"jsonrpc"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(notif); err != nil {
		logDebug("json encode error: %v", err)
		return
	}
	_ = os.Stdout.Sync()
}
