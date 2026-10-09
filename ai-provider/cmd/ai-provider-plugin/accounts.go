package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lum1n/smuler/plugins/sdk-go"
)

// --- Usage windows (rendered as host gauges) ---

// usageWindow is a platform-neutral usage meter. Percent is 0..100. Severity
// is filled in by buildSnapshot from the configured thresholds; the caption
// ("resets in …") is computed when the item is built so cached readings stay
// accurate.
type usageWindow struct {
	ID         string
	Label      string
	Percent    float64
	ValueLabel string
	ResetAt    time.Time
	Caption    string // used when ResetAt is zero
	Severity   string
}

func newWindow(id, label string, pct float64, resetAt time.Time) usageWindow {
	return usageWindow{ID: id, Label: label, Percent: clampPercent(pct), ResetAt: resetAt}
}

// amountWindow builds a gauge for a used/total amount, e.g. "$4.20 / $10".
func amountWindow(id, label string, used, total float64, format func(float64) string, resetAt time.Time) (usageWindow, bool) {
	if total <= 0 {
		return usageWindow{}, false
	}
	used = max(0, used)
	w := newWindow(id, label, used/total*100, resetAt)
	w.ValueLabel = format(used) + " / " + format(total)
	return w, true
}

func clampPercent(pct float64) float64 {
	if math.IsNaN(pct) {
		return 0
	}
	return max(0, min(100, pct))
}

// formatUSD renders "$10" for whole amounts and "$4.20" otherwise.
func formatUSD(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("$%.0f", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

func formatCount(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func (w usageWindow) gauge() sdk.Gauge {
	caption := w.Caption
	if !w.ResetAt.IsZero() {
		caption = "resets in " + durationUntil(w.ResetAt)
	}
	return sdk.Gauge{
		ID:         w.ID,
		Label:      w.Label,
		Value:      clampPercent(w.Percent) / 100,
		ValueLabel: w.ValueLabel,
		Caption:    caption,
		Severity:   w.Severity,
	}
}

// --- Job planning (dedupe by effective credential) ---

type providerJob struct {
	provider Provider
	auth     AuthContext
	hasAuth  bool
	key      string
	slot     string // account id or record index; used in item/alert IDs
	noSecret bool   // no usable host secret (local fallback or auth missing)
	order    int
}

// credentialIdentity returns the effective credential a record will use, so
// records that would fetch the same account collapse into one job. usable is
// false when the record carries no usable host secret.
func (p *aiProviderPlugin) credentialIdentity(provider Provider, auth AuthContext) (identity string, usable bool) {
	if provider.ID() == "claude" {
		creds, ok, _ := claudeSettingsCredentials(auth)
		if !ok {
			// No token, or an API key that cannot read plan usage: Fetch falls
			// back to the local Claude Code login.
			return "local", false
		}
		if cp, isClaude := provider.(*claudeProvider); isClaude {
			if local := cp.knownLocalToken(); local != "" && local == creds.AccessToken {
				return "local", false
			}
		}
		return "token:" + creds.AccessToken, true
	}
	token := authToken(auth)
	cookie := strings.Join(strings.Fields(auth.CookieHeader), " ")
	if token == "" && cookie == "" {
		return "", false
	}
	return "token:" + token + "\x00cookie:" + cookie, true
}

func (p *aiProviderPlugin) planJobs(enabledSet map[string]bool) []providerJob {
	jobs := make([]providerJob, 0)
	for _, provider := range p.providers {
		id := provider.ID()
		if !enabledSet[id] {
			continue
		}
		requiresHost := providerRequiresHostAuth(id)
		auths := p.authsForProvider(id)
		if len(auths) == 0 {
			jobs = append(jobs, providerJob{provider: provider, hasAuth: !requiresHost, key: id, noSecret: true})
			continue
		}
		seen := make(map[string]bool)
		var withSecret, withoutSecret []providerJob
		for i, auth := range auths {
			identity, usable := p.credentialIdentity(provider, auth)
			if seen[identity] {
				continue
			}
			seen[identity] = true
			slot := auth.AccountID
			if slot == "" {
				slot = strconv.Itoa(i)
			}
			job := providerJob{
				provider: provider,
				auth:     auth,
				hasAuth:  usable || !requiresHost,
				key:      id + ":" + slot,
				slot:     slot,
				noSecret: !usable,
			}
			if usable {
				withSecret = append(withSecret, job)
			} else {
				withoutSecret = append(withoutSecret, job)
			}
		}
		// Providers that need a host secret: an empty record only yields an
		// auth_required card when no record has a secret at all.
		if requiresHost && len(withSecret) > 0 {
			withoutSecret = nil
		}
		jobs = append(jobs, withoutSecret...)
		jobs = append(jobs, withSecret...)
	}
	for i := range jobs {
		jobs[i].order = i
		if jobs[i].slot == "0" && jobs[i].auth.AccountID == "" {
			// Keep the historical item ID for a single unnamed record.
			jobs[i].slot = ""
		}
	}
	return jobs
}

// --- Post-fetch card processing ---

// finalizeCards collapses cards that turned out to be the same account, drops
// redundant auth_required cards, orders cards stably, assigns account labels
// and gauge severities.
func (p *aiProviderPlugin) finalizeCards(cards []ProviderStatus) []ProviderStatus {
	sort.SliceStable(cards, func(i, j int) bool { return cards[i].order < cards[j].order })

	byProvider := make(map[string][]ProviderStatus)
	var providerOrder []string
	for _, c := range cards {
		if _, ok := byProvider[c.ProviderID]; !ok {
			providerOrder = append(providerOrder, c.ProviderID)
		}
		byProvider[c.ProviderID] = append(byProvider[c.ProviderID], c)
	}

	out := make([]ProviderStatus, 0, len(cards))
	for _, pid := range providerOrder {
		group := collapseGroup(byProvider[pid])
		group = dropRedundantAuthCards(group)
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].AccountID != group[j].AccountID {
				return group[i].AccountID < group[j].AccountID
			}
			return group[i].order < group[j].order
		})
		assignAccountLabels(group)
		out = append(out, group...)
	}

	for i := range out {
		if len(out[i].Windows) == 0 {
			continue
		}
		windows := make([]usageWindow, len(out[i].Windows))
		for j, w := range out[i].Windows {
			w.Severity = p.severityFor(w.Percent)
			windows[j] = w
		}
		out[i].Windows = windows
	}

	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := providerPriority[out[i].ProviderID], providerPriority[out[j].ProviderID]
		return pi < pj
	})
	return out
}

func collapseGroup(group []ProviderStatus) []ProviderStatus {
	seen := make(map[string]bool)
	kept := make([]ProviderStatus, 0, len(group))
	for _, c := range group {
		var keys []string
		if c.IdentityKey != "" {
			keys = append(keys, "id:"+c.IdentityKey)
		}
		if c.CredentialKey != "" {
			keys = append(keys, "cred:"+c.CredentialKey)
		}
		dup := false
		for _, k := range keys {
			if seen[k] {
				dup = true
			}
		}
		if dup {
			continue
		}
		for _, k := range keys {
			seen[k] = true
		}
		kept = append(kept, c)
	}
	return kept
}

func dropRedundantAuthCards(group []ProviderStatus) []ProviderStatus {
	anyWorking := false
	for _, c := range group {
		if c.Health != "auth_required" {
			anyWorking = true
			break
		}
	}
	if !anyWorking {
		return group
	}
	kept := make([]ProviderStatus, 0, len(group))
	for _, c := range group {
		if c.Health == "auth_required" && c.noSecret {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// assignAccountLabels picks each card's subtitle label: provider-reported
// identity, else the host label (unless it just repeats the provider name),
// else "Account N" when the provider has several cards. Labels are made unique
// within the provider.
func assignAccountLabels(group []ProviderStatus) {
	for i := range group {
		c := &group[i]
		label := strings.TrimSpace(c.AccountIdentity)
		if label == "" {
			host := strings.TrimSpace(c.AccountDisplayName)
			if host != "" && !strings.EqualFold(host, c.DisplayName) && !strings.EqualFold(host, c.ProviderID) {
				label = host
			}
		}
		if label == "" && len(group) > 1 {
			label = "Account " + strconv.Itoa(i+1)
		}
		c.AccountLabel = label
	}
	if len(group) < 2 {
		return
	}
	counts := make(map[string]int)
	for _, c := range group {
		counts[c.AccountLabel]++
	}
	used := make(map[string]bool)
	for i := range group {
		c := &group[i]
		if counts[c.AccountLabel] > 1 {
			if suffix := shortSuffix(c.AccountID); suffix != "" {
				c.AccountLabel += " · " + suffix
			}
		}
		base := c.AccountLabel
		for n := 2; used[c.AccountLabel]; n++ {
			c.AccountLabel = base + " · " + strconv.Itoa(n)
		}
		used[c.AccountLabel] = true
	}
}

func shortSuffix(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if len(id) > 6 {
		return id[len(id)-6:]
	}
	return id
}

// credentialFingerprint is a non-reversible key for comparing credentials.
func credentialFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:8])
}
