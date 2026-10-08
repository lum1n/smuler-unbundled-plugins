package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var devinOrgName = ""

func providerRequiresHostAuth(providerID string) bool {
	switch providerID {
	case "gemini", "kiro", "claude":
		// These fall back to local CLI credentials when no host secret is set.
		return false
	default:
		return true
	}
}

// --- Gemini Provider ---

type geminiProvider struct{}

func (p *geminiProvider) ID() string          { return "gemini" }
func (p *geminiProvider) DisplayName() string { return "Gemini" }

type geminiOAuthCreds struct {
	AccessToken  string
	RefreshToken string
	ExpiryDate   time.Time
	IDToken      string
}

func geminiHomeDir() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return os.Getenv("HOME")
}

func geminiLoadOAuthCreds() (geminiOAuthCreds, error) {
	path := filepath.Join(geminiHomeDir(), ".gemini", "oauth_creds.json")
	body, err := os.ReadFile(path)
	if err != nil {
		return geminiOAuthCreds{}, err
	}
	return geminiParseOAuthCreds(body)
}

func geminiParseOAuthCreds(body []byte) (geminiOAuthCreds, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return geminiOAuthCreds{}, err
	}
	creds := geminiOAuthCreds{}
	if v, ok := raw["access_token"].(string); ok {
		creds.AccessToken = strings.TrimSpace(v)
	}
	if v, ok := raw["refresh_token"].(string); ok {
		creds.RefreshToken = strings.TrimSpace(v)
	}
	if v, ok := raw["id_token"].(string); ok {
		creds.IDToken = strings.TrimSpace(v)
	}
	// gemini-cli writes expiry_date as epoch milliseconds (a JSON number).
	switch v := raw["expiry_date"].(type) {
	case float64:
		if v > 0 {
			creds.ExpiryDate = time.UnixMilli(int64(v)).UTC()
		}
	case string:
		if ms, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && ms > 0 {
			creds.ExpiryDate = time.UnixMilli(ms).UTC()
		} else {
			creds.ExpiryDate = parseFlexibleTime(v)
		}
	}
	return creds, nil
}

func geminiFindOAuthClient() (clientID, clientSecret string, ok bool) {
	home := geminiHomeDir()
	candidates := []string{
		"/usr/local/lib/node_modules/@google/gemini-cli/node_modules/@google/gemini-cli-core/dist/src/code_assist/oauth2.js",
		"/opt/homebrew/lib/node_modules/@google/gemini-cli/node_modules/@google/gemini-cli-core/dist/src/code_assist/oauth2.js",
		filepath.Join(home, ".npm-global/lib/node_modules/@google/gemini-cli/node_modules/@google/gemini-cli-core/dist/src/code_assist/oauth2.js"),
	}
	reClient := regexp.MustCompile(`OAUTH_CLIENT_ID\s*=\s*['"]([^'"]+)['"]`)
	reSecret := regexp.MustCompile(`OAUTH_CLIENT_SECRET\s*=\s*['"]([^'"]+)['"]`)
	for _, path := range candidates {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(body)
		clientMatch := reClient.FindStringSubmatch(text)
		secretMatch := reSecret.FindStringSubmatch(text)
		if len(clientMatch) == 2 && len(secretMatch) == 2 {
			return clientMatch[1], secretMatch[1], true
		}
	}
	return "", "", false
}

func geminiRefreshAccessToken(ctx context.Context, refreshToken string) (string, error) {
	clientID, clientSecret, ok := geminiFindOAuthClient()
	if !ok {
		return "", fmt.Errorf("Gemini CLI OAuth configuration not found")
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", formatHTTPError("Gemini", resp.StatusCode, body)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", err
	}
	token, _ := parsed["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("Gemini token refresh missing access_token")
	}
	return strings.TrimSpace(token), nil
}

func geminiResolveAccessToken(ctx context.Context, auth AuthContext) (string, error) {
	if token := strings.TrimSpace(authToken(auth)); token != "" {
		return token, nil
	}
	creds, err := geminiLoadOAuthCreds()
	if err != nil {
		return "", err
	}
	if creds.AccessToken != "" && (creds.ExpiryDate.IsZero() || creds.ExpiryDate.After(time.Now().Add(30*time.Second))) {
		return creds.AccessToken, nil
	}
	if creds.RefreshToken == "" {
		return "", fmt.Errorf("Gemini not logged in")
	}
	return geminiRefreshAccessToken(ctx, creds.RefreshToken)
}

func geminiDiscoverProjectID(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return ""
	}
	var parsed struct {
		Projects []struct {
			ProjectID string            `json:"projectId"`
			Labels    map[string]string `json:"labels"`
		} `json:"projects"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	for _, project := range parsed.Projects {
		if strings.HasPrefix(project.ProjectID, "gen-lang-client") {
			return project.ProjectID
		}
		if project.Labels["generative-language"] != "" {
			return project.ProjectID
		}
	}
	return ""
}

func geminiLowestQuotaPercent(body []byte) (float64, time.Time, error) {
	var parsed struct {
		Buckets []struct {
			ModelID           string   `json:"modelId"`
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, time.Time{}, err
	}
	if len(parsed.Buckets) == 0 {
		return 0, time.Time{}, fmt.Errorf("Gemini quota response missing buckets")
	}
	bestLeft := 101.0
	var resetAt time.Time
	for _, bucket := range parsed.Buckets {
		if bucket.RemainingFraction == nil {
			continue
		}
		left := *bucket.RemainingFraction * 100
		if left < bestLeft {
			bestLeft = left
			resetAt = parseFlexibleTime(bucket.ResetTime)
		}
	}
	if bestLeft > 100 {
		return 0, time.Time{}, fmt.Errorf("Gemini quota response had no usable buckets")
	}
	return max(0, min(100, 100-bestLeft)), resetAt, nil
}

func (p *geminiProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	accessToken, err := geminiResolveAccessToken(ctx, auth)
	if err != nil {
		return ProviderStatus{
			ProviderID:  "gemini",
			DisplayName: "Gemini",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Gemini",
			Details:     "Run `gemini` in Terminal to authenticate, or paste an access token.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	projectID := geminiDiscoverProjectID(ctx, accessToken)
	payload := "{}"
	if projectID != "" {
		payload = fmt.Sprintf(`{"project":"%s"}`, projectID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", strings.NewReader(payload))
	if err != nil {
		return ProviderStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
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
			ProviderID:  "gemini",
			DisplayName: "Gemini",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Gemini",
			Details:     "Gemini credentials expired. Re-authenticate with `gemini`.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	if resp.StatusCode >= 400 {
		return ProviderStatus{}, formatHTTPError("Gemini", resp.StatusCode, body)
	}

	usagePct, resetAt, err := geminiLowestQuotaPercent(body)
	if err != nil {
		return ProviderStatus{}, err
	}
	details := fmt.Sprintf("%.0f%% used", usagePct)
	if !resetAt.IsZero() {
		details += ", resets in " + durationUntil(resetAt)
	}
	return ProviderStatus{
		ProviderID:     "gemini",
		DisplayName:    "Gemini",
		UsagePercent:   usagePct,
		RemainingLabel: fmt.Sprintf("%.0f%% used", usagePct),
		WindowLabel:    "Model quotas",
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://aistudio.google.com",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Windsurf Provider ---

type windsurfProvider struct{}

func (p *windsurfProvider) ID() string          { return "windsurf" }
func (p *windsurfProvider) DisplayName() string { return "Windsurf" }

type windsurfSessionAuth struct {
	SessionToken string
	Auth1Token   string
	AccountID    string
	PrimaryOrgID string
}

func windsurfParseSession(raw string) (windsurfSessionAuth, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return windsurfSessionAuth{}, fmt.Errorf("empty windsurf session")
	}
	if strings.HasPrefix(raw, "{") {
		var values map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return windsurfSessionAuth{}, err
		}
		return windsurfSessionFromMap(values)
	}
	values := map[string]interface{}{}
	for _, segment := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == ',' || r == ';' }) {
		segment = strings.TrimSpace(strings.Trim(segment, "{}"))
		parts := strings.SplitN(segment, "=", 2)
		if len(parts) != 2 {
			parts = strings.SplitN(segment, ":", 2)
		}
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		values[key] = value
	}
	return windsurfSessionFromMap(values)
}

func windsurfSessionFromMap(values map[string]interface{}) (windsurfSessionAuth, error) {
	get := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := values[key]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	auth := windsurfSessionAuth{
		SessionToken: get("devin_session_token", "devinSessionToken", "sessionToken"),
		Auth1Token:   get("devin_auth1_token", "devinAuth1Token", "auth1Token"),
		AccountID:    get("devin_account_id", "devinAccountId", "accountID", "accountId"),
		PrimaryOrgID: get("devin_primary_org_id", "devinPrimaryOrgId", "primaryOrgID", "primaryOrgId"),
	}
	if auth.SessionToken == "" || auth.Auth1Token == "" || auth.AccountID == "" || auth.PrimaryOrgID == "" {
		return windsurfSessionAuth{}, fmt.Errorf("missing windsurf session fields")
	}
	return auth, nil
}

func windsurfFetchPlanStatus(ctx context.Context, auth windsurfSessionAuth) (windsurfPlanStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://windsurf.com/_backend/exa.seat_management_pb.SeatManagementService/GetPlanStatus", strings.NewReader(string(windsurfEncodePlanStatusRequest(auth.SessionToken))))
	if err != nil {
		return windsurfPlanStatus{}, err
	}
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Origin", "https://windsurf.com")
	req.Header.Set("Referer", "https://windsurf.com/profile")
	req.Header.Set("x-auth-token", auth.SessionToken)
	req.Header.Set("x-devin-session-token", auth.SessionToken)
	req.Header.Set("x-devin-auth1-token", auth.Auth1Token)
	req.Header.Set("x-devin-account-id", auth.AccountID)
	req.Header.Set("x-devin-primary-org-id", auth.PrimaryOrgID)
	resp, err := httpClient.Do(req)
	if err != nil {
		return windsurfPlanStatus{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return windsurfPlanStatus{}, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return windsurfPlanStatus{}, fmt.Errorf("session expired or invalid")
	}
	if resp.StatusCode >= 400 {
		return windsurfPlanStatus{}, formatHTTPError("Windsurf", resp.StatusCode, body)
	}
	return windsurfDecodePlanStatusResponse(body)
}

func (p *windsurfProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	session, err := windsurfParseSession(auth.CookieHeader)
	if err != nil {
		return ProviderStatus{
			ProviderID:  "windsurf",
			DisplayName: "Windsurf",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Windsurf",
			Details:     "Paste Windsurf session JSON from browser localStorage.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	status, err := windsurfFetchPlanStatus(ctx, session)
	if err != nil {
		if strings.Contains(err.Error(), "session expired") {
			return ProviderStatus{
				ProviderID:  "windsurf",
				DisplayName: "Windsurf",
				Health:      "auth_required",
				Severity:    "info",
				WindowLabel: "Windsurf",
				Details:     "Session expired or invalid. Re-import session JSON.",
				Timestamp:   time.Now().UTC(),
			}, nil
		}
		return ProviderStatus{}, err
	}

	dailyUsed := 0.0
	weeklyUsed := 0.0
	if status.DailyQuotaRemainingPercent > 0 {
		dailyUsed = max(0, min(100, 100-float64(status.DailyQuotaRemainingPercent)))
	}
	if status.WeeklyQuotaRemainingPercent > 0 {
		weeklyUsed = max(0, min(100, 100-float64(status.WeeklyQuotaRemainingPercent)))
	}
	usagePct := dailyUsed
	resetAt := status.DailyResetAt
	if weeklyUsed > usagePct {
		usagePct = weeklyUsed
		resetAt = status.WeeklyResetAt
	}
	windowLabel := strings.TrimSpace(status.PlanName)
	if windowLabel == "" {
		windowLabel = "Windsurf"
	}
	details := fmt.Sprintf("Daily %.0f%%, Weekly %.0f%%", dailyUsed, weeklyUsed)
	if !resetAt.IsZero() {
		details += ", resets in " + durationUntil(resetAt)
	}
	return ProviderStatus{
		ProviderID:     "windsurf",
		DisplayName:    "Windsurf",
		UsagePercent:   usagePct,
		RemainingLabel: details,
		WindowLabel:    windowLabel,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://windsurf.com/profile",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Grok Provider ---

type grokProvider struct{}

func (p *grokProvider) ID() string          { return "grok" }
func (p *grokProvider) DisplayName() string { return "Grok" }

func grokFetchBilling(ctx context.Context, authorizationHeader, cookieHeader string) (grokBillingSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig", strings.NewReader("\x00\x00\x00\x00\x00"))
	if err != nil {
		return grokBillingSnapshot{}, err
	}
	if authorizationHeader != "" {
		req.Header.Set("Authorization", authorizationHeader)
	}
	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}
	req.Header.Set("Origin", "https://grok.com")
	req.Header.Set("Referer", "https://grok.com/?_s=usage")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	req.Header.Set("x-grpc-web", "1")
	req.Header.Set("x-user-agent", "connect-es/2.1.1")
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return grokBillingSnapshot{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return grokBillingSnapshot{}, err
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return grokBillingSnapshot{}, fmt.Errorf("session expired or invalid")
	}
	if resp.StatusCode >= 400 {
		return grokBillingSnapshot{}, formatHTTPError("Grok", resp.StatusCode, body)
	}
	return grokParseBillingResponse(body, time.Now().UTC())
}

func (p *grokProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	token := strings.TrimSpace(authToken(auth))
	cookie := strings.TrimSpace(auth.CookieHeader)
	if token == "" && cookie == "" {
		return ProviderStatus{
			ProviderID:  "grok",
			DisplayName: "Grok",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Grok",
			Details:     "Import a grok.com browser session or paste an access token.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	authHeader := ""
	if token != "" {
		authHeader = "Bearer " + token
	}
	snapshot, err := grokFetchBilling(ctx, authHeader, cookie)
	if err != nil {
		if strings.Contains(err.Error(), "session expired") {
			return ProviderStatus{
				ProviderID:  "grok",
				DisplayName: "Grok",
				Health:      "auth_required",
				Severity:    "info",
				WindowLabel: "Grok",
				Details:     "Session expired or invalid. Re-import from browser.",
				Timestamp:   time.Now().UTC(),
			}, nil
		}
		return ProviderStatus{}, err
	}
	details := fmt.Sprintf("%.0f%% used", snapshot.UsedPercent)
	if !snapshot.ResetAt.IsZero() {
		details += ", resets in " + durationUntil(snapshot.ResetAt)
	}
	return ProviderStatus{
		ProviderID:     "grok",
		DisplayName:    "Grok",
		UsagePercent:   snapshot.UsedPercent,
		RemainingLabel: details,
		WindowLabel:    "Grok credits",
		ResetAt:        snapshot.ResetAt,
		Severity:       severityForPercent(snapshot.UsedPercent),
		Health:         "ready",
		DeepLink:       "https://grok.com",
		SummaryValue:   fmt.Sprintf("%.0f%%", snapshot.UsedPercent),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

// --- Augment Provider ---

type augmentProvider struct{}

func (p *augmentProvider) ID() string          { return "augment" }
func (p *augmentProvider) DisplayName() string { return "Augment" }

func (p *augmentProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	cookie := strings.TrimSpace(auth.CookieHeader)
	if cookie == "" {
		return ProviderStatus{
			ProviderID:  "augment",
			DisplayName: "Augment",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Augment",
			Details:     "Import a browser session from app.augmentcode.com",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	creditsBody, creditsStatus, err := augmentFetch(ctx, "https://app.augmentcode.com/api/credits", cookie)
	if err != nil {
		return ProviderStatus{}, err
	}
	if creditsStatus == 401 || creditsStatus == 403 {
		return ProviderStatus{
			ProviderID:  "augment",
			DisplayName: "Augment",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Augment",
			Details:     "Session expired or invalid. Re-import from browser.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	if creditsStatus >= 400 {
		return ProviderStatus{}, formatHTTPError("Augment", creditsStatus, creditsBody)
	}
	var credits struct {
		UsageUnitsRemaining                *float64 `json:"usageUnitsRemaining"`
		UsageUnitsConsumedThisBillingCycle *float64 `json:"usageUnitsConsumedThisBillingCycle"`
		UsageUnitsAvailable                *float64 `json:"usageUnitsAvailable"`
	}
	if err := json.Unmarshal(creditsBody, &credits); err != nil {
		return ProviderStatus{}, fmt.Errorf("Augment parse error: %w", err)
	}

	limit := 0.0
	if credits.UsageUnitsAvailable != nil && *credits.UsageUnitsAvailable > 0 {
		limit = *credits.UsageUnitsAvailable
	} else if credits.UsageUnitsRemaining != nil && credits.UsageUnitsConsumedThisBillingCycle != nil {
		limit = *credits.UsageUnitsRemaining + *credits.UsageUnitsConsumedThisBillingCycle
	}
	used := 0.0
	if credits.UsageUnitsConsumedThisBillingCycle != nil {
		used = *credits.UsageUnitsConsumedThisBillingCycle
	}
	usagePct := 0.0
	if limit > 0 {
		usagePct = min(100, (used/limit)*100)
	}

	subBody, subStatus, _ := augmentFetch(ctx, "https://app.augmentcode.com/api/subscription", cookie)
	planName := "Augment"
	var resetAt time.Time
	if subStatus == 200 {
		var sub struct {
			PlanName         string `json:"planName"`
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		}
		if json.Unmarshal(subBody, &sub) == nil {
			if strings.TrimSpace(sub.PlanName) != "" {
				planName = strings.TrimSpace(sub.PlanName)
			}
			resetAt = parseFlexibleTime(sub.BillingPeriodEnd)
		}
	}

	details := fmt.Sprintf("%.0f%% used", usagePct)
	if !resetAt.IsZero() {
		details += ", resets in " + durationUntil(resetAt)
	}
	return ProviderStatus{
		ProviderID:     "augment",
		DisplayName:    "Augment",
		UsagePercent:   usagePct,
		RemainingLabel: details,
		WindowLabel:    planName,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://app.augmentcode.com",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

func augmentFetch(ctx context.Context, url, cookie string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

// --- Factory Provider ---

type factoryProvider struct{}

func (p *factoryProvider) ID() string          { return "factory" }
func (p *factoryProvider) DisplayName() string { return "Factory" }

func factoryBearerFromCookie(cookie string) string {
	for _, chunk := range strings.Split(cookie, ";") {
		trimmed := strings.TrimSpace(chunk)
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if (name == "access-token" || strings.Contains(name, "session-token") || name == "session") && strings.Contains(value, ".") {
			return value
		}
	}
	return ""
}

func (p *factoryProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	cookie := strings.TrimSpace(auth.CookieHeader)
	token := strings.TrimSpace(authToken(auth))
	if token == "" {
		token = factoryBearerFromCookie(cookie)
	}
	if cookie == "" && token == "" {
		return ProviderStatus{
			ProviderID:  "factory",
			DisplayName: "Factory",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Factory",
			Details:     "Import a browser session from app.factory.ai",
			Timestamp:   time.Now().UTC(),
		}, nil
	}

	body, status, err := factoryFetchLimits(ctx, cookie, token)
	if err != nil {
		return ProviderStatus{}, err
	}
	if status == 401 || status == 403 {
		return ProviderStatus{
			ProviderID:  "factory",
			DisplayName: "Factory",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Factory",
			Details:     "Session expired or invalid. Re-import from browser.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	if status >= 400 {
		return ProviderStatus{}, formatHTTPError("Factory", status, body)
	}

	usagePct, resetAt, windowLabel, details, err := factoryParseLimits(body)
	if err != nil {
		return ProviderStatus{}, err
	}
	return ProviderStatus{
		ProviderID:     "factory",
		DisplayName:    "Factory",
		UsagePercent:   usagePct,
		RemainingLabel: details,
		WindowLabel:    windowLabel,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://app.factory.ai",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

func factoryFetchLimits(ctx context.Context, cookie, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.factory.ai/api/billing/limits", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://app.factory.ai")
	req.Header.Set("Referer", "https://app.factory.ai/")
	req.Header.Set("x-factory-client", "web-app")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func factoryParseLimits(body []byte) (usagePct float64, resetAt time.Time, windowLabel, details string, err error) {
	var parsed map[string]interface{}
	if err = json.Unmarshal(body, &parsed); err != nil {
		return 0, time.Time{}, "", "", fmt.Errorf("Factory parse error: %w", err)
	}
	limits, _ := parsed["limits"].(map[string]interface{})
	if limits == nil {
		return 0, time.Time{}, "Factory", "No billing limits available", nil
	}
	bestPct := 0.0
	bestLabel := "Factory"
	for _, poolName := range []string{"standard", "core"} {
		pool, _ := limits[poolName].(map[string]interface{})
		if pool == nil {
			continue
		}
		for _, windowName := range []string{"fiveHour", "weekly", "monthly"} {
			window, _ := pool[windowName].(map[string]interface{})
			if window == nil {
				continue
			}
			pct, _ := commandCodeDoubleValue(window["usedPercent"])
			if pct > bestPct {
				bestPct = pct
				bestLabel = titleCase(poolName) + " " + windowName
				if sec, ok := commandCodeDoubleValue(window["secondsRemaining"]); ok && sec > 0 {
					resetAt = time.Now().Add(time.Duration(sec) * time.Second)
				}
			}
		}
	}
	details = fmt.Sprintf("%.0f%% used", bestPct)
	if !resetAt.IsZero() {
		details += ", resets in " + durationUntil(resetAt)
	}
	return min(100, bestPct), resetAt, bestLabel, details, nil
}

// --- Zed Provider ---

type zedProvider struct{}

func (p *zedProvider) ID() string          { return "zed" }
func (p *zedProvider) DisplayName() string { return "Zed" }

func (p *zedProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	token := strings.TrimSpace(authToken(auth))
	if token == "" {
		return ProviderStatus{
			ProviderID:  "zed",
			DisplayName: "Zed",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Zed",
			Details:     "Paste your Zed access token from Keychain or Zed settings.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloud.zed.dev/client/users/me", nil)
	if err != nil {
		return ProviderStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
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
			ProviderID:  "zed",
			DisplayName: "Zed",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Zed",
			Details:     "Token expired or invalid.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	if resp.StatusCode >= 400 {
		return ProviderStatus{}, formatHTTPError("Zed", resp.StatusCode, body)
	}
	usagePct, resetAt, planName, details, err := zedParseUsage(body)
	if err != nil {
		return ProviderStatus{}, err
	}
	return ProviderStatus{
		ProviderID:     "zed",
		DisplayName:    "Zed",
		UsagePercent:   usagePct,
		RemainingLabel: details,
		WindowLabel:    planName,
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://zed.dev",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

func zedParseUsage(body []byte) (float64, time.Time, string, string, error) {
	var parsed struct {
		Plan struct {
			Name               string `json:"name"`
			SubscriptionPeriod *struct {
				StartedAt string `json:"startedAt"`
				EndedAt   string `json:"endedAt"`
			} `json:"subscriptionPeriod"`
			Usage struct {
				EditPredictions struct {
					Used  int `json:"used"`
					Limit int `json:"limit"`
				} `json:"editPredictions"`
			} `json:"usage"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, time.Time{}, "", "", fmt.Errorf("Zed parse error: %w", err)
	}
	used := parsed.Plan.Usage.EditPredictions.Used
	limit := parsed.Plan.Usage.EditPredictions.Limit
	usagePct := 0.0
	if limit > 0 {
		usagePct = min(100, (float64(used)/float64(limit))*100)
	}
	resetAt := time.Time{}
	if parsed.Plan.SubscriptionPeriod != nil {
		resetAt = parseFlexibleTime(parsed.Plan.SubscriptionPeriod.EndedAt)
	}
	planName := strings.TrimSpace(parsed.Plan.Name)
	if planName == "" {
		planName = "Zed"
	}
	details := fmt.Sprintf("%d / %d edit predictions", used, limit)
	if !resetAt.IsZero() {
		details += ", resets in " + durationUntil(resetAt)
	}
	return usagePct, resetAt, planName, details, nil
}

// --- Warp Provider ---

type warpProvider struct{}

func (p *warpProvider) ID() string          { return "warp" }
func (p *warpProvider) DisplayName() string { return "Warp" }

func (p *warpProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	apiKey := strings.TrimSpace(authToken(auth))
	if apiKey == "" {
		return ProviderStatus{
			ProviderID:  "warp",
			DisplayName: "Warp",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Warp",
			Details:     "Paste your Warp API key.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	body, status, err := warpFetch(ctx, apiKey)
	if err != nil {
		return ProviderStatus{}, err
	}
	if status == 401 || status == 403 {
		return ProviderStatus{
			ProviderID:  "warp",
			DisplayName: "Warp",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Warp",
			Details:     "API key expired or invalid.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	if status >= 400 {
		return ProviderStatus{}, formatHTTPError("Warp", status, body)
	}
	usagePct, resetAt, details, err := warpParseUsage(body)
	if err != nil {
		return ProviderStatus{}, err
	}
	return ProviderStatus{
		ProviderID:     "warp",
		DisplayName:    "Warp",
		UsagePercent:   usagePct,
		RemainingLabel: details,
		WindowLabel:    "Request credits",
		ResetAt:        resetAt,
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://app.warp.dev",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        details,
		Timestamp:      time.Now().UTC(),
	}, nil
}

func warpFetch(ctx context.Context, apiKey string) ([]byte, int, error) {
	query := `query GetRequestLimitInfo($requestContext: RequestContext!) { user(requestContext: $requestContext) { __typename ... on UserOutput { user { requestLimitInfo { isUnlimited nextRefreshTime requestLimit requestsUsedSinceLastRefresh } } } } } }`
	payload := map[string]interface{}{
		"operationName": "GetRequestLimitInfo",
		"query":         query,
		"variables": map[string]interface{}{
			"requestContext": map[string]interface{}{
				"clientContext": map[string]interface{}{},
				"osContext": map[string]interface{}{
					"category": "macOS",
					"name":     "macOS",
					"version":  "14.0.0",
				},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://app.warp.dev/graphql/v2?op=GetRequestLimitInfo", strings.NewReader(string(encoded)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-warp-client-id", "warp-app")
	req.Header.Set("x-warp-os-category", "macOS")
	req.Header.Set("x-warp-os-name", "macOS")
	req.Header.Set("x-warp-os-version", "14.0.0")
	req.Header.Set("User-Agent", "Warp/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func warpParseUsage(body []byte) (float64, time.Time, string, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, time.Time{}, "", fmt.Errorf("Warp parse error: %w", err)
	}
	info := nestedMap(parsed, "data", "user", "user", "requestLimitInfo")
	if info == nil {
		return 0, time.Time{}, "", fmt.Errorf("Warp response missing requestLimitInfo")
	}
	if unlimited, _ := info["isUnlimited"].(bool); unlimited {
		return 0, time.Time{}, "Unlimited", nil
	}
	used, _ := commandCodeDoubleValue(info["requestsUsedSinceLastRefresh"])
	limit, _ := commandCodeDoubleValue(info["requestLimit"])
	usagePct := 0.0
	if limit > 0 {
		usagePct = min(100, (used/limit)*100)
	}
	resetAt := parseFlexibleTime(fmt.Sprint(info["nextRefreshTime"]))
	details := fmt.Sprintf("%.0f / %.0f requests", used, limit)
	if !resetAt.IsZero() {
		details += ", resets in " + durationUntil(resetAt)
	}
	return usagePct, resetAt, details, nil
}

// --- Devin Provider ---

type devinProvider struct{}

func (p *devinProvider) ID() string          { return "devin" }
func (p *devinProvider) DisplayName() string { return "Devin" }

func devinNormalizeOrg(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "devin.ai") {
		if idx := strings.Index(raw, "/org/"); idx >= 0 {
			parts := strings.Split(strings.TrimPrefix(raw[idx+1:], "org/"), "/")
			if len(parts) > 0 && parts[0] != "" {
				return "org/" + parts[0]
			}
		}
	}
	raw = strings.Trim(raw, "/")
	if strings.HasPrefix(raw, "org/") || strings.HasPrefix(raw, "organizations/") {
		return raw
	}
	if strings.HasPrefix(raw, "org-") || strings.HasPrefix(raw, "org_") {
		return "organizations/" + raw
	}
	return "org/" + raw
}

func (p *devinProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	token := strings.TrimSpace(authToken(auth))
	if token == "" {
		return ProviderStatus{
			ProviderID:  "devin",
			DisplayName: "Devin",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Devin",
			Details:     "Paste a Devin bearer token.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	org := devinNormalizeOrg(devinOrgName)
	if org == "" {
		return ProviderStatus{
			ProviderID:  "devin",
			DisplayName: "Devin",
			Health:      "degraded",
			Severity:    "info",
			WindowLabel: "Devin",
			Details:     "Set Devin org slug in plugin settings.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	paths := []string{
		org + "/billing/quota/usage",
		strings.TrimPrefix(org, "org/") + "/billing/quota/usage",
	}
	var lastErr error
	for _, path := range paths {
		body, status, err := devinFetch(ctx, path, token)
		if err != nil {
			lastErr = err
			continue
		}
		if status == 401 || status == 403 {
			return ProviderStatus{
				ProviderID:  "devin",
				DisplayName: "Devin",
				Health:      "auth_required",
				Severity:    "info",
				WindowLabel: "Devin",
				Details:     "Token expired or invalid.",
				Timestamp:   time.Now().UTC(),
			}, nil
		}
		if status >= 400 {
			lastErr = formatHTTPError("Devin", status, body)
			continue
		}
		daily, weekly, planName, err := devinParseUsage(body)
		if err != nil {
			lastErr = err
			continue
		}
		usagePct := daily
		if weekly > usagePct {
			usagePct = weekly
		}
		details := fmt.Sprintf("Daily %.0f%%, Weekly %.0f%%", daily, weekly)
		if planName != "" {
			details = planName + " · " + details
		}
		return ProviderStatus{
			ProviderID:     "devin",
			DisplayName:    "Devin",
			UsagePercent:   usagePct,
			RemainingLabel: details,
			WindowLabel:    "Devin quota",
			Severity:       severityForPercent(usagePct),
			Health:         "ready",
			DeepLink:       "https://app.devin.ai",
			SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
			Details:        details,
			Timestamp:      time.Now().UTC(),
		}, nil
	}
	if lastErr != nil {
		return ProviderStatus{}, lastErr
	}
	return ProviderStatus{}, fmt.Errorf("Devin quota fetch failed")
}

func devinFetch(ctx context.Context, path, token string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://app.devin.ai/api/"+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "smuler-ai-provider-plugin/0.1.1")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func devinParseUsage(body []byte) (daily float64, weekly float64, planName string, err error) {
	var parsed interface{}
	if err = json.Unmarshal(body, &parsed); err != nil {
		return 0, 0, "", err
	}
	daily = devinFindWindowPercent(parsed, "daily")
	weekly = devinFindWindowPercent(parsed, "weekly")
	planName = devinFindString(parsed, "plan", "planName", "name")
	if daily == 0 && weekly == 0 {
		return 0, 0, "", fmt.Errorf("Devin response missing quota windows")
	}
	return daily, weekly, planName, nil
}

func devinFindWindowPercent(value interface{}, kind string) float64 {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, kind) {
				if pct := devinExtractPercent(child); pct > 0 {
					return pct
				}
			}
			if pct := devinFindWindowPercent(child, kind); pct > 0 {
				return pct
			}
		}
	case []interface{}:
		for _, child := range typed {
			if pct := devinFindWindowPercent(child, kind); pct > 0 {
				return pct
			}
		}
	}
	return 0
}

func devinExtractPercent(value interface{}) float64 {
	switch typed := value.(type) {
	case map[string]interface{}:
		for _, key := range []string{"usedPercent", "usagePercent", "percentUsed", "percent"} {
			if pct, ok := commandCodeDoubleValue(typed[key]); ok {
				return min(100, pct)
			}
		}
		if used, ok := commandCodeDoubleValue(typed["used"]); ok {
			if limit, ok := commandCodeDoubleValue(typed["limit"]); ok && limit > 0 {
				return min(100, (used/limit)*100)
			}
		}
	}
	return 0
}

func devinFindString(value interface{}, keys ...string) string {
	switch typed := value.(type) {
	case map[string]interface{}:
		for _, key := range keys {
			if raw, ok := typed[key].(string); ok && strings.TrimSpace(raw) != "" {
				return strings.TrimSpace(raw)
			}
		}
		for _, child := range typed {
			if found := devinFindString(child, keys...); found != "" {
				return found
			}
		}
	case []interface{}:
		for _, child := range typed {
			if found := devinFindString(child, keys...); found != "" {
				return found
			}
		}
	}
	return ""
}

// --- Kiro Provider ---

type kiroProvider struct{}

func (p *kiroProvider) ID() string          { return "kiro" }
func (p *kiroProvider) DisplayName() string { return "Kiro" }

var kiroPercentRE = regexp.MustCompile(`(?i)(\d+)%`)

func (p *kiroProvider) Fetch(ctx context.Context, auth AuthContext) (ProviderStatus, error) {
	if _, err := exec.LookPath("kiro-cli"); err != nil {
		return ProviderStatus{
			ProviderID:  "kiro",
			DisplayName: "Kiro",
			Health:      "degraded",
			Severity:    "info",
			WindowLabel: "Kiro",
			Details:     "kiro-cli not found. Install from https://kiro.dev",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "kiro-cli", "chat", "--no-interactive", "/usage")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	output, err := cmd.CombinedOutput()
	text := stripANSI(string(output))
	lower := strings.ToLower(text)
	if err != nil || strings.Contains(lower, "not logged in") || strings.Contains(lower, "kiro-cli login") {
		return ProviderStatus{
			ProviderID:  "kiro",
			DisplayName: "Kiro",
			Health:      "auth_required",
			Severity:    "info",
			WindowLabel: "Kiro",
			Details:     "Run `kiro-cli login` first.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	match := kiroPercentRE.FindStringSubmatch(text)
	if len(match) < 2 {
		return ProviderStatus{
			ProviderID:  "kiro",
			DisplayName: "Kiro",
			Health:      "degraded",
			Severity:    "info",
			WindowLabel: "Kiro",
			Details:     "Could not parse kiro-cli usage output.",
			Timestamp:   time.Now().UTC(),
		}, nil
	}
	usagePct, _ := strconv.ParseFloat(match[1], 64)
	usagePct = min(100, usagePct)
	return ProviderStatus{
		ProviderID:     "kiro",
		DisplayName:    "Kiro",
		UsagePercent:   usagePct,
		RemainingLabel: fmt.Sprintf("%.0f%% used", usagePct),
		WindowLabel:    "Kiro credits",
		Severity:       severityForPercent(usagePct),
		Health:         "ready",
		DeepLink:       "https://kiro.dev",
		SummaryValue:   fmt.Sprintf("%.0f%%", usagePct),
		Details:        fmt.Sprintf("%.0f%% used", usagePct),
		Timestamp:      time.Now().UTC(),
	}, nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func parseFlexibleTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func nestedMap(root map[string]interface{}, keys ...string) map[string]interface{} {
	current := interface{}(root)
	for _, key := range keys {
		asMap, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current = asMap[key]
	}
	asMap, ok := current.(map[string]interface{})
	if !ok {
		return nil
	}
	return asMap
}

func stripANSI(text string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	return re.ReplaceAllString(text, "")
}
