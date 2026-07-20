# AI Provider Plugin Plan

## Goal

Build a first-party `ai-provider` plugin for smuler that shows real usage-limit data for these MVP providers:

- Codex
- Claude
- OpenRouter
- OpenCode
- OpenCode Go

The plugin must:

- show one card per provider in the AI Providers dropdown section
- show only the single provider closest to its usage limit in the menubar
- use real provider data, not fixtures or inferred placeholders
- use OAuth or API keys where supported
- fall back to browser session import only when OAuth or API key access is not enough

## Product Shape

### Menubar behavior

- The menubar label should show exactly one provider summary from this plugin.
- The displayed provider is the enabled provider currently closest to exhaustion.
- Selection should prefer the provider with the highest normalized usage percentage.
- If no provider has usable live data, show a degraded plugin summary such as `AI auth` or `AI unavailable`.

Examples:

- `Claude 82%`
- `Codex 68%`
- `OpenRouter $7 left`

### Dropdown behavior

- The AI Providers plugin renders as one plugin section in the dropdown.
- Inside that section, render one item card per provider.
- Each card should show:
  - provider name
  - plan or window label
  - current usage or remaining balance
  - reset time when available
  - auth or health state when degraded
- Cards should deep-link to the provider's relevant usage or billing page when possible.

## MVP Provider Matrix

### Codex

- Auth strategy: OAuth
- Data target: primary and secondary usage windows with reset times
- Notes:
  - This is Codex quota usage, not generic OpenAI API billing.
  - The plugin should target the same class of usage data that Codex surfaces for user limits.

### Claude

- Auth strategy: OAuth
- Data target: session and weekly usage windows with reset times
- Notes:
  - OAuth is the preferred path for real usage-limit data.

### OpenRouter

- Auth strategy: API key
- Data target:
  - credit balance
  - key limits when available
  - daily, weekly, and monthly spend when exposed
- Notes:
  - This provider does not require OAuth.

### OpenCode

- Auth strategy: browser session import
- Data target:
  - rolling usage window
  - weekly usage window
  - reset times
- Notes:
  - Session import is allowed here because OAuth and API key support are not sufficient.

### OpenCode Go

- Auth strategy: browser session import
- Data target:
  - Go-specific usage windows for the selected workspace
  - reset times
- Notes:
  - Reuse the same OpenCode session source where possible.

## Architecture Decision

Auth should be implemented as a reusable host capability, not embedded fully inside the plugin.

The right abstraction is not an OAuth-only component. It should be a broader host-owned credentials system.

### Host-owned responsibilities

- secure credential storage in macOS Keychain
- OAuth login flows
- API key storage and validation lifecycle
- browser session import and secure storage
- account connect, disconnect, and reconnect UI
- auth-state tracking
- diagnostics redaction
- scoped auth delivery to plugins

### Plugin-owned responsibilities

- provider-specific API calls and parsing
- provider-specific normalization into a shared usage model
- threshold evaluation
- summary selection for the menubar
- dropdown item generation

## Required Host Changes

The current host auth/runtime model is too narrow for this plugin.

### 1. Add reusable CredentialsManager

Create a host service that stores and manages multiple credentials per plugin.

Suggested stored record shape:

- `pluginId`
- `providerId`
- `accountId`
- `kind` (`oauth`, `apiKey`, `webSession`)
- `displayName`
- `secretPayload`
- `expiresAt`
- `metadata`

Key behaviors:

- Keychain-backed storage
- save, load, update, delete
- redaction-safe diagnostics output
- notify view model and runtime when credentials change

### 2. Add provider auth definitions in host

The host should have first-party definitions for each supported AI provider.

Each definition should include:

- `providerId`
- display name
- auth kind
- settings copy
- OAuth scopes if applicable
- callback or local redirect metadata for OAuth providers
- validation rules for API keys or session imports

### 3. Expand plugin protocol auth payload

Current protocol only sends one `auth.accountId` string during `initialize`, which is not enough.

Add support for multiple auth contexts per plugin.

Suggested runtime auth shape:

- `providerId`
- `accountId`
- `kind`
- `accessToken`
- `apiKey`
- `cookieHeader`
- `expiresAt`
- `displayName`

Guidelines:

- only send the minimum material required by the plugin at runtime
- keep refresh tokens host-side if possible
- restart plugin on credential changes in v1

### 4. Add Settings UI for credentials

Extend Preferences so first-party providers can be connected from the host.

Required MVP controls:

- Codex: Connect with OAuth / Disconnect
- Claude: Connect with OAuth / Disconnect
- OpenRouter: Save API key / Remove API key
- OpenCode: Import browser session / Clear session
- OpenCode Go: workspace selector plus reuse OpenCode session

### 5. Add browser session import support

This should be a reusable host service, not duplicated inside the plugin.

Required capabilities:

- import cookies for supported domains
- securely store imported session material
- validate that a session is still usable
- support manual re-import
- expose session age and validity state to the host UI

For MVP, only implement the minimum needed for `opencode.ai`.

## Plugin Design

### Identity

- Plugin ID: `ai-provider`
- Display name: `AI Providers`
- Language: Go

### Why Go

- consistent with the existing real plugin implementation pattern in this repo
- single binary distribution
- straightforward stdio JSON-RPC implementation
- easy concurrency for fetching multiple providers in parallel

### Suggested layout

```text
plugins/ai-provider/
  README.md
  go.mod
  cmd/
    ai-provider-plugin/
      main.go
  internal/
    config/
    providers/
    snapshot/
    timefmt/
```

### Internal provider interface

Use one provider adapter per supported provider.

Suggested interface:

```go
type Provider interface {
    ID() string
    Fetch(ctx context.Context, auth AuthContext, cfg ProviderConfig) (ProviderStatus, error)
}
```

### Normalized provider status model

Each adapter should normalize its result into a shared internal model.

Suggested fields:

- `ProviderID`
- `DisplayName`
- `UsagePercent`
- `RemainingLabel`
- `WindowLabel`
- `ResetAt`
- `Severity`
- `Health`
- `DeepLink`
- `SummaryValue`
- `Details`

Notes:

- `UsagePercent` should be the main field used for menubar winner selection.
- Providers without a clean percentage should map to a best-effort comparable signal.
- If a provider cannot yield a comparable percent, it can still render a dropdown card but should not beat providers with valid normalized percentages for the menubar.

## Summary Selection Rules

The menubar should show exactly one provider from the AI Providers plugin.

Selection algorithm:

1. Ignore disabled providers.
2. Ignore providers with unusable data for normalized comparison.
3. Prefer providers in `critical` severity.
4. Otherwise select the provider with highest `UsagePercent`.
5. Break ties by earliest `ResetAt`.
6. Break remaining ties by stable provider priority:
   - Codex
   - Claude
   - OpenCode Go
   - OpenCode
   - OpenRouter

Fallback behavior:

- If every enabled provider is missing auth, show `AI auth` with `auth_required` health.
- If some providers are live and others are auth-missing, show the best live provider and include auth-missing providers as cards.
- If every provider errors, show a degraded summary and include per-provider error cards.

## Dropdown Card Rules

Each provider card should contain:

- title: provider name
- subtitle: plan or usage window
- detail line: usage plus reset countdown
- severity color based on threshold
- deep link to usage or billing page

Examples:

- `Claude` / `Weekly usage` / `82% used, resets in 1d 4h`
- `Codex` / `Primary window` / `68% used, resets in 2h 10m`
- `OpenRouter` / `Credit balance` / `$7.21 remaining`
- `OpenCode` / `Rolling usage` / `54% used, resets in 3h 11m`

## Alert Rules

Default thresholds:

- warning at `75%`
- critical at `90%`

Alert conditions:

- provider crosses warning threshold
- provider crosses critical threshold
- provider enters auth-required state
- provider refresh repeatedly fails after previously healthy state

Avoid alert spam:

- alert only on threshold transitions
- do not repeat every refresh
- do not notify for initial startup state unless explicitly severe and new

## Refresh Policy

Plugin refresh policy:

- fetch all enabled providers concurrently
- use per-provider timeouts
- return partial success if one or more providers fail
- recommend refresh interval around `300` seconds by default

Host behavior for v1:

- restart plugin on credential updates
- keep last-known-good snapshot if a provider refresh fails

## Error and Degraded-State Behavior

Per-provider failures must not break the whole plugin.

Per-provider states:

- `ready`
- `auth_required`
- `degraded`
- `error`

Examples:

- missing OAuth token for Claude: card shows `auth required`
- expired OpenCode session: card shows `session expired`
- OpenRouter API timeout: card shows stale or unavailable state but other cards continue to render

Aggregate plugin health:

- `ok` if at least one provider is healthy and none are critical failures
- `degraded` if some providers fail but at least one works
- `auth_required` if all enabled providers lack usable auth
- `error` if all enabled providers fail for non-auth reasons

## Implementation Phases

### Phase 0: Feasibility lock

Goal:

- confirm the live data paths to use for all MVP providers before writing the plugin

Deliverables:

- chosen auth strategy per provider
- chosen usage endpoint or session source per provider
- list of required host-side credential capabilities

### Phase 1: Host credentials foundation

Goal:

- add reusable host support for mixed credentials

Deliverables:

- `CredentialsManager`
- Keychain-backed multi-credential storage
- settings UI shell for provider credentials
- runtime notifications on credential changes

### Phase 2: Protocol and runtime expansion

Goal:

- make the runtime able to pass rich auth contexts into a plugin

Deliverables:

- expanded plugin protocol models
- host runtime wiring
- restart-on-auth-change flow

### Phase 3: Provider implementations

Goal:

- implement the five provider adapters

Recommended order:

1. OpenRouter
2. Claude
3. Codex
4. OpenCode
5. OpenCode Go

Rationale:

- OpenRouter is the simplest live-data path.
- Claude and Codex are the highest-value OAuth-backed providers.
- OpenCode session import is more specialized and should build on the host session substrate.

### Phase 4: AI plugin aggregation and UX

Goal:

- combine provider outputs into one polished plugin section

Deliverables:

- one card per provider
- closest-to-limit menubar summary
- threshold-based alerts
- degraded-state handling

### Phase 5: Reliability and polish

Goal:

- make the feature usable daily

Deliverables:

- timeout tuning
- retry and caching behavior
- diagnostics without secret leakage
- auth repair UX

## Files To Add Or Change

### Host

- `packages/plugin-protocol/Sources/PluginProtocol/ProtocolMessages.swift`
- `packages/plugin-protocol/Sources/PluginProtocol/PluginManifest.swift`
- `apps/macos-host/Sources/SmulerMacOSHost/Services/ManagedPluginRuntime.swift`
- `apps/macos-host/Sources/SmulerMacOSHost/Services/KeychainStore.swift`
- new host credentials services
- `apps/macos-host/Sources/SmulerMacOSHost/Views/SettingsView.swift`
- `apps/macos-host/Sources/SmulerMacOSHost/ViewModels/MenuBarViewModel.swift`

### New plugin

- `plugins/ai-provider/README.md`
- `plugins/ai-provider/go.mod`
- `plugins/ai-provider/cmd/ai-provider-plugin/main.go`
- `plugins/ai-provider/manifest.json`

## Non-Goals For MVP

- generic third-party plugin auth SDK
- multi-account switching inside the AI provider UI
- historical charts in smuler dropdown cards
- browser-session import for providers outside the MVP list
- Linux host parity

## Acceptance Criteria

The MVP is done when:

- all required providers return real live data
- Codex works via OAuth
- Claude works via OAuth
- OpenRouter works via API key
- OpenCode works via browser session import
- OpenCode Go works via browser session import
- the AI Providers section renders one card per provider
- the menubar shows only the provider closest to its limit
- missing or expired credentials surface clearly as auth-required state
- credential changes can be repaired from Settings without editing files manually
- secrets are stored securely and not logged

## Final Recommendation

Build the reusable host credentials foundation first.

Do not start by writing only the plugin binary. The plugin depends on richer auth and runtime support than the current host provides. The smallest correct path is:

1. host credentials subsystem
2. protocol/runtime auth expansion
3. provider adapters
4. AI Providers plugin UX
