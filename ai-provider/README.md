# AI Provider Plugin

First-party plugin for AI coding provider usage limits and quota tracking.

Current responsibilities:
- Usage tracking for seventeen AI providers: Augment, Claude, Codex, Command Code, Copilot, Cursor, Devin, Factory, Gemini, Grok, Kiro, OpenCode, OpenCode Go, OpenRouter, Warp, Windsurf, and Zed
- One card per provider in the dropdown
- Menubar summary showing the provider closest to its usage limit
- Threshold-based alerts at 75% (warning) and 90% (critical)
- Deep links to provider usage/billing pages

Implementation notes:
- Built as a Go binary at `bin/ai-provider-plugin`
- Uses JSON-RPC over stdio
- Talks to each provider's API for live usage data
- Requires host-level credential management (OAuth, API keys, browser sessions) except Gemini (CLI creds fallback) and Kiro (`kiro-cli`)

Providers are declared in `manifest.json` under `authProviders` (OAuth, API key, browser session / cookie import, etc.) and appear in Settings. Connect a provider there to enable it.

Current config keys:
- `enabledProviders` - comma-separated list of provider IDs (managed by the host from Settings)
- `warningThreshold` - percentage threshold for warning severity (default 75)
- `criticalThreshold` - percentage threshold for critical severity (default 90)
- `copilotAccountType` - account type for Copilot: `personal` (default) or `enterprise`
- `copilotOrgName` - GitHub org slug for enterprise Copilot seat info
- `copilotEnterpriseHost` - GitHub Enterprise Server hostname (leave empty for github.com)
- `devinOrgName` - Devin organization slug for quota API calls
