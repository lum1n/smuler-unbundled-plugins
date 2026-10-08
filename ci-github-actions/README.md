# GitHub Actions Plugin

First-party plugin for GitHub Actions and CI workflow signals.

## Responsibilities

- Failing workflows across auto-discovered repositories
- Recent critical failures sorted by recency
- Deep links into workflow runs
- Delta event notifications for new failures and recoveries

## Protocol

Long-lived JSON-RPC process over stdio. Written in Go.

### Initialize
Receives the GitHub token via `params.providerAuths[].accessToken` / `apiKey`, falling back to the legacy `params.auth.accountId` (populated from macOS Keychain by the host).

### getStatus / refresh
1. Fetches the authenticated user's repositories via `GET /user/repos?sort=pushed` and skips repos not pushed in the last 14 days
2. For each remaining repo, fetches the 20 most recent workflow runs via `GET /repos/{owner}/{repo}/actions/runs`
3. A workflow/branch counts as failing only when its latest completed run (within 14 days) failed, timed out, or failed to start
4. Fetches runs concurrently (up to 8 repos at a time) with conditional requests (`If-None-Match`), under a 25s overall budget
5. Reports:
   - Count of failing workflows
   - Up to 10 most recent failures with repo, branch, and deep link
   - Critical alert when failures are present

If the token is missing or the API is unreachable, reports a degraded state with appropriate messaging.

### Events
- `workflow.failed` — new workflow run has failed
- `workflow.fixed` — previously failing workflow is now passing
- `failure.count_increased` — total failure count grew
- `failure.count_decreased` — total failure count shrank

## Build

```sh
make ci-github-actions-build
```
