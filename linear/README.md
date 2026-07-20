# Linear Plugin

First-party plugin for Linear workflow signals.

Current responsibilities:
- Assigned issues
- Mentions
- Optional team triage for configured teams
- Priority and due-state visibility
- Workflow state summaries
- Deep links to Linear issues

Implementation notes:
- Built as a Go binary at `bin/linear-plugin`
- Uses JSON-RPC over stdio
- Talks to the Linear GraphQL API

Current config keys:
- `showAssigned`
- `showMentions`
- `showTriage`
- `teamIds`
