# Agent Monitor

Registry plugin that surfaces local coding-agent state to smuler.

## How classification works

1. If tmux is running and `@agent_watcher_socket` (or `{socket_path}.agent-watcher.sock`) is live, Agent Monitor **attaches** to that [agent-watcher](https://github.com/lum1n/agent-watcher) daemon — the same one [tmux-agent-state](https://github.com/lum1n/tmux-agent-state) and Sessh use.
2. If tmux is running and no socket is live, it **starts** the bundled watcher (`third_party/agent-watcher`, installed as `watcher/agent_watcher.py`, Python 3.10+) with `--listen` on the canonical tmux socket path and sets `@agent_watcher_socket`.
3. If tmux is not running, it does not start a watcher. A process scan plus shell/native hooks still find agents in Terminal, iTerm, Ghostty, and other non-tmux shells.

PIDs already owned by a watcher pane are not duplicated as `kind-pid` rows.

## Requirements

- **Always:** local process table (macOS `ps` / Linux `/proc`).
- **Tmux agents:** `tmux` 3.2+. Shared daemon if tmux-agent-state or Sessh already started one.
- **When this plugin starts the daemon:** `python3` 3.10+ and the files copied to `~/.config/smuler/plugins/agent-monitor/watcher/` by `make install`.

## Settings

| Key | Default | Purpose |
|-----|---------|---------|
| `agent.watcherEnabled` | `true` | Attach or spawn agent-watcher |
| `agent.pythonPath` | `python3` | Interpreter for a smuler-owned daemon |
| `agent.pollInterval` | `5` | Non-tmux process scan interval (seconds) |

The classifier sources live in `third_party/agent-watcher` (git submodule). That path is used instead of `vendor/` so Go module vendoring is not triggered.

Install tmux-agent-state if you want one classifier shared with Sessh and the tmux statusline.
