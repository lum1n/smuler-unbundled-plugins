package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/email/internal"
	"github.com/lum1n/smuler/plugins/email/internal/imap"
	"github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "email"
	pluginVersion = "0.1.0"
)

// maxNewMailEvents caps notifications per account per refresh so a burst of
// incoming mail (or a mailbox resync) does not flood the user.
const maxNewMailEvents = 3

type fetchResult struct {
	messages []email.EmailMessage
	total    int
	err      error
	account  email.Account
}

type handler struct {
	mu             sync.Mutex
	accounts       []email.Account
	providers      []email.MailProvider
	refreshSeconds int
	maxItems       int
}

func (h *handler) Initialize(params sdk.InitializeParams) string {
	h.refreshSeconds = 60
	if v, ok := params.Config["refreshSeconds"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			h.refreshSeconds = n
		}
	}
	h.maxItems = 10
	if v, ok := params.Config["maxItems"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			h.maxItems = n
		}
	}

	pwdByAccount := mapPasswords(params.ProviderAuths)
	accounts := parseAccounts(params.Config, pwdByAccount)
	applyDefaultPassword(accounts, params)

	var providers []email.MailProvider
	for _, acc := range accounts {
		prov := imap.New(acc)
		providers = append(providers, prov)
	}

	h.mu.Lock()
	old := h.providers
	h.accounts = accounts
	h.providers = providers
	h.mu.Unlock()

	// Re-initialize must not leak the previous IMAP sessions.
	for _, prov := range old {
		_ = prov.Close()
	}

	// Connect accounts concurrently so one slow server does not delay the
	// handshake for the others (each dial/login is bounded by timeouts).
	errs := make([]error, len(providers))
	var wg sync.WaitGroup
	for i, prov := range providers {
		wg.Add(1)
		go func(idx int, p email.MailProvider) {
			defer wg.Done()
			if err := p.Connect(); err != nil {
				sdk.Log("connect %s: %v", p.Account().ID, err)
				errs[idx] = err
			}
		}(i, prov)
	}
	wg.Wait()

	connected, authFailed := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			connected++
		case errors.Is(err, email.ErrAuthFailed):
			authFailed++
		}
	}

	if connected == 0 && len(providers) > 0 {
		if authFailed == len(providers) {
			return sdk.HealthAuthReq
		}
		return sdk.HealthError
	}
	if connected < len(providers) {
		return sdk.HealthDegraded
	}
	return sdk.HealthOK
}

func (h *handler) GetStatus() sdk.Snapshot {
	h.mu.Lock()
	providers := make([]email.MailProvider, len(h.providers))
	copy(providers, h.providers)
	maxItems := h.maxItems
	h.mu.Unlock()

	var wg sync.WaitGroup
	results := make([]fetchResult, len(providers))

	for i, prov := range providers {
		wg.Add(1)
		go func(idx int, p email.MailProvider) {
			defer wg.Done()
			res, err := p.FetchUnread(context.Background(), maxItems)
			results[idx] = fetchResult{
				messages: res.Messages,
				total:    res.Total,
				err:      err,
				account:  p.Account(),
			}
			if err != nil {
				sdk.Log("fetch %s: %v", p.Account().ID, err)
				return
			}

			newUIDs := res.NewUIDs
			if len(newUIDs) > maxNewMailEvents {
				newUIDs = newUIDs[len(newUIDs)-maxNewMailEvents:]
			}
			for _, uid := range newUIDs {
				sdk.Emit(sdk.Event{
					Type:     "email.new_message",
					Severity: sdk.SeverityInfo,
					Message:  fmt.Sprintf("New email in %s", p.Account().Label),
					Data:     map[string]string{"accountId": p.Account().ID, "uid": strconv.FormatUint(uint64(uid), 10)},
				})
			}
		}(i, prov)
	}
	wg.Wait()

	return h.buildSnapshot(results)
}

func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) < 2 {
		return false, "invalid action id: " + id
	}
	action := parts[0]
	accountID := parts[1]

	h.mu.Lock()
	defer h.mu.Unlock()

	var prov email.MailProvider
	for _, p := range h.providers {
		if p.Account().ID == accountID {
			prov = p
			break
		}
	}
	if prov == nil {
		return false, "account not found: " + accountID
	}

	switch action {
	case "open":
		url := email.WebmailURL(prov.Account().Provider)
		if url == "" {
			return false, "no webmail url for provider"
		}
		if err := exec.Command("open", url).Run(); err != nil {
			return false, fmt.Sprintf("failed to open browser: %v", err)
		}
		return true, ""

	case "read":
		uid := parseUID(parts)
		if uid == 0 {
			return false, "invalid uid"
		}
		if err := prov.MarkRead(uid); err != nil {
			return false, fmt.Sprintf("mark read failed: %v", err)
		}
		return true, ""

	case "archive":
		uid := parseUID(parts)
		if uid == 0 {
			return false, "invalid uid"
		}
		if err := prov.Archive(uid); err != nil {
			return false, fmt.Sprintf("archive failed: %v", err)
		}
		return true, ""

	default:
		return false, "unknown action: " + action
	}
}

func parseUID(parts []string) uint32 {
	if len(parts) < 3 {
		return 0
	}
	n, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

func (h *handler) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, prov := range h.providers {
		_ = prov.Close()
	}
}

func (h *handler) buildSnapshot(results []fetchResult) sdk.Snapshot {
	var allMessages []email.EmailMessage
	var alerts []sdk.Alert
	totalUnread := 0
	errorCount := 0
	authCount := 0

	for _, r := range results {
		if r.err != nil {
			errorCount++
			message := fmt.Sprintf("Cannot connect to %s: %v", r.account.Label, r.err)
			if errors.Is(r.err, email.ErrAuthFailed) {
				authCount++
				message = fmt.Sprintf("Sign-in failed for %s — check the username and app password", r.account.Label)
			}
			alerts = append(alerts, sdk.Alert{
				ID:       fmt.Sprintf("error-%s", r.account.ID),
				Severity: sdk.SeverityWarning,
				Message:  message,
			})
			continue
		}
		totalUnread += r.total
		allMessages = append(allMessages, r.messages...)
	}

	sort.Slice(allMessages, func(i, j int) bool {
		return allMessages[i].ReceivedAt.After(allMessages[j].ReceivedAt)
	})

	items := make([]sdk.Item, 0, h.maxItems)
	for i, m := range allMessages {
		if i >= h.maxItems {
			break
		}
		items = append(items, messageToItem(m))
	}

	if len(items) == 0 && errorCount == 0 {
		items = append(items, sdk.Item{
			ID:       "no-unread",
			Title:    "No unread messages",
			Subtitle: "Inbox zero across all accounts",
			Severity: sdk.SeverityInfo,
			Actions:  []sdk.Action{},
		})
	}

	state := sdk.StateReady
	health := sdk.HealthOK
	severity := sdk.SeverityInfo

	if errorCount == len(results) && len(results) > 0 {
		state = sdk.StateError
		health = sdk.HealthError
		severity = sdk.SeverityCritical
		if authCount == errorCount {
			health = sdk.HealthAuthReq
		}
	} else if errorCount > 0 {
		state = sdk.StateDegraded
		health = sdk.HealthDegraded
		severity = sdk.SeverityWarning
	}

	unreadValue := "Inbox zero"
	if totalUnread > 0 {
		unreadValue = fmt.Sprintf("%d unread", totalUnread)
		if totalUnread > 5 {
			severity = sdk.SeverityWarning
		}
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    state,
		Summary: sdk.Summary{
			Title:    "Email",
			Value:    unreadValue,
			Severity: severity,
			Trend:    sdk.TrendSteady,
			IconHint: "mail",
		},
		Items:        items,
		Actions:      []sdk.Action{},
		Alerts:       alerts,
		RefreshAfter: h.refreshSeconds,
		Health:       health,
	}
}

func messageToItem(m email.EmailMessage) sdk.Item {
	subtitle := m.From
	if m.AccountLabel != "" {
		subtitle = m.AccountLabel + " \u00b7 " + m.From
	}

	detail := m.Snippet
	if detail == "" {
		detail = m.Subject
	}

	timestamp := ""
	if !m.ReceivedAt.IsZero() {
		timestamp = m.ReceivedAt.Format(time.RFC3339)
	}

	actions := []sdk.Action{
		{ID: fmt.Sprintf("open:%s", m.AccountID), Label: "Open Webmail"},
		{ID: fmt.Sprintf("read:%s:%d", m.AccountID, m.UID), Label: "Mark Read"},
		{ID: fmt.Sprintf("archive:%s:%d", m.AccountID, m.UID), Label: "Archive"},
	}

	return sdk.Item{
		ID:        fmt.Sprintf("msg-%s-%d", m.AccountID, m.UID),
		Title:     m.Subject,
		Subtitle:  subtitle,
		Detail:    detail,
		Severity:  sdk.SeverityInfo,
		Timestamp: timestamp,
		DeepLink:  m.WebLink,
		Actions:   actions,
		Metadata: map[string]string{
			"isUnread": "true",
			"sender":   m.From,
			"subject":  m.Subject,
		},
	}
}

func parseAccounts(config map[string]string, passwords map[string]string) []email.Account {
	var accounts []email.Account

	if countStr, ok := config["accountCount"]; ok {
		count := email.ParseInt(countStr)
		for i := 0; i < count; i++ {
			prefix := fmt.Sprintf("account.%d.", i)
			acc := parseAccount(config, prefix, passwords)
			if acc.ID != "" {
				accounts = append(accounts, acc)
			}
		}
	}

	if len(accounts) == 0 {
		acc := parseAccount(config, "", passwords)
		if acc.ID != "" {
			accounts = append(accounts, acc)
		}
	}

	return accounts
}

func parseAccount(config map[string]string, prefix string, passwords map[string]string) email.Account {
	get := func(key string) string {
		return config[prefix+key]
	}

	provider := get("provider")
	if provider == "" {
		if prefix == "" {
			return email.Account{}
		}
		provider = email.ProviderIMAP
	}

	accountID := get("id")
	if accountID == "" {
		accountID = "default"
	}

	label := get("label")
	username := get("username")
	server := get("imapServer")
	port := get("imapPort")
	tls := get("tls")
	passwd := ""

	if pw, ok := passwords[accountID]; ok {
		passwd = pw
	}
	if passwd == "" {
		passwd = get("password")
	}

	return email.AccountFromConfig(accountID, provider, label, username, server, port, tls, passwd)
}

// applyDefaultPassword fills in the password for a single configured account
// from the plugin-level API key the host passes in Auth.AccountID (the
// Settings "api_key" credential), which was previously ignored.
func applyDefaultPassword(accounts []email.Account, params sdk.InitializeParams) {
	if len(accounts) != 1 || accounts[0].Password != "" {
		return
	}
	if params.Auth != nil {
		if pw := strings.TrimSpace(params.Auth.AccountID); pw != "" {
			accounts[0].Password = pw
			return
		}
	}
	var keys []string
	for _, a := range params.ProviderAuths {
		if a.Kind == "api_key" && a.APIKey != "" {
			keys = append(keys, a.APIKey)
		}
	}
	if len(keys) == 1 {
		accounts[0].Password = keys[0]
	}
}

func mapPasswords(auths []sdk.ProviderAuthContext) map[string]string {
	result := make(map[string]string)
	for _, a := range auths {
		if a.Kind == "api_key" && a.APIKey != "" {
			result[a.AccountID] = a.APIKey
		}
	}
	return result
}

func main() {
	sdk.Run(pluginID, pluginVersion, &handler{})
}
