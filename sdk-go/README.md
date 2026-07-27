# smuler Go plugin SDK

Reference SDK for building smuler plugins in Go (JSON-RPC 2.0 over stdio).

## Quick start

```go
package main

import sdk "github.com/lum1n/smuler/plugins/sdk-go"

type handler struct{}

func (h *handler) Initialize(params sdk.InitializeParams) string { return sdk.HealthOK }
func (h *handler) GetStatus() sdk.Snapshot { /* ... */ return sdk.Snapshot{} }
func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
    return true, ""
}
func (h *handler) Shutdown() {}

func main() {
    sdk.Run("my-plugin", "0.1.0", &handler{})
}
```

Declare `"actions"` in `manifest.json` capabilities when you handle `performAction`.
Declare `"windows"` when any action may return a host-rendered window.

## Opening a themed host window

Plugins never draw UI. Return structured `window` content and the host renders it with the active theme.

Implement optional `ActionResultHandler` (preferred over `(bool, string)`):

```go
func (h *handler) PerformActionResult(id string, params map[string]string) sdk.ActionResult {
    switch id {
    case "show-notes":
        return sdk.ActionWindow(sdk.WindowContent{
            ID:       "my-plugin.notes",
            Title:    "Notes",
            Subtitle: "My Plugin",
            IconHint: "doc.text",
            Sections: []sdk.WindowSection{{
                ID:    "main",
                Title: "Highlights",
                Blocks: []sdk.WindowBlock{
                    {ID: "1", Text: "First point", Style: sdk.WindowBlockBullet},
                },
            }},
        })
    default:
        return sdk.ActionFail("unknown action")
    }
}

func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
    return false, "use PerformActionResult"
}
```

Helpers: `ActionOK`, `ActionFail`, `ActionWindow`, `ActionAIWindow`, `ActionAITask`.

## Local AI assist (host-defined tasks)

Ask the host to run a well-known local AI task and update your window:

```go
return sdk.ActionAITask(sdk.AIWindowOpts{
    ID:       "my-plugin.risk.1",
    Title:    "PR title",
    Subtitle: "My Plugin",
    IconHint: "exclamationmark.triangle",
    Task:     sdk.AITaskRiskReview, // or Explain, TriageNext, DraftReply, ExtractActions, SummarizeBullets
    Input:    sourceText,
})
```

| Constant | Task id | Output |
|----------|---------|--------|
| `AITaskSummarizeBullets` | `summarize_bullets` | bullets |
| `AITaskExplain` | `explain` | paragraphs |
| `AITaskRiskReview` | `risk_review` | bullets |
| `AITaskTriageNext` | `triage_next` | bullets |
| `AITaskDraftReply` | `draft_reply` | paragraphs |
| `AITaskExtractActions` | `extract_actions` | bullets |

The host owns prompts, truncation, and theming. Plugins supply `input` only.

## Schema / protocol

- Manifest: `docs/plugin-manifest-schema.json` (`capabilities` includes `windows`)
- Action result: `docs/plugin-action-result-schema.json`
- Protocol overview: `docs/architecture/plugin-protocol.md`
