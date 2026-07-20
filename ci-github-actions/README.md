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
Receives the GitHub token via `params.providerAuths[].accountId` (populated from macOS Keychain by the host).

### getStatus / refresh
1. Fetches the authenticated user's repositories via `GET /user/repos?sort=pushed`
2. For each repo, fetches recent failing workflow runs via `GET /repos/{owner}/{repo}/actions/runs?status=failure`
3. Fetches run concurrently (up to 5 repos at a time) to stay responsive
4. Reports:
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
