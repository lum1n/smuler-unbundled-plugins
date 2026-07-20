# GitHub Plugin

First-party **registry** plugin for GitHub workflow signals (not bundled with the host).

## Responsibilities

- Review requests (PRs awaiting your review)
- Pull request state
- Deep links to open PRs directly
- Delta event notifications with issue URLs for tap-to-open

## Protocol

Long-lived JSON-RPC process over stdio. Written in Go.

### Initialize
Receives the GitHub token via `params.providerAuths[].accountId` (populated from macOS Keychain by the host).

### getStatus / refresh
Calls `GET /search/issues?q=is:pr+is:open+review-requested:@me` to find PRs awaiting review. Reports:

- Review request count
- Up to 3 most recent PRs with title, repo, and deep link
- Warning alert if review requests exist

If the token is missing or the API is unreachable, reports a degraded state with appropriate messaging.

### Events
- `pr.review_requested` — new PR added to review queue, includes URL
- `pr.review_completed` — PR removed from review queue, includes URL
- `pr.count_increased` — backlog grew
- `pr.count_decreased` — backlog shrank

## Build / install

```sh
make -C plugins/github build
make -C plugins/github install   # ~/.config/smuler/plugins/github
```

Or from the repo root: `make registry-plugins-install`.
