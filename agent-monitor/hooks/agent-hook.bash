_agent_monitor_sock="${HOME}/.cache/smuler/agent-monitor.sock"

_agent_monitor_send() {
  if [ -S "$_agent_monitor_sock" ]; then
    echo "$1" | nc -U "$_agent_monitor_sock" 2>/dev/null & disown
  fi
}

# Print the agent id for a command line, or nothing. Matches the command word
# exactly so e.g. "ping" or "pip" are not mistaken for "pi".
_agent_monitor_agent_for() {
  case "$1" in
    archer|archer\ *)     echo archer ;;
    claude|claude\ *)     echo claude ;;
    opencode|opencode\ *) echo opencode ;;
    codex|codex\ *)       echo codex ;;
    pi|pi\ *)             echo pi ;;
    aider|aider\ *)       echo aider ;;
  esac
}

_agent_monitor_prompt_command() {
  local last_exit=$?
  local hist
  hist="$(history 1)"
  # Only react to a new history entry; an empty Enter re-runs PROMPT_COMMAND
  # with the same last command and used to emit duplicate agent_end events.
  if [ "$hist" != "$_agent_monitor_last_hist" ]; then
    _agent_monitor_last_hist="$hist"
    local cmd agent_id
    cmd="$(printf '%s' "$hist" | sed 's/^[ ]*[0-9]*[ ]*//')"
    agent_id="$(_agent_monitor_agent_for "$cmd")"
    if [ -n "$agent_id" ]; then
      _agent_monitor_send "{\"type\":\"agent_end\",\"agentId\":\"$agent_id\",\"exitCode\":$last_exit,\"pid\":$$}"
    fi
  fi
  return $last_exit
}

_agent_monitor_last_hist="$(history 1)"
case ";${PROMPT_COMMAND:-};" in
  *";_agent_monitor_prompt_command;"*) ;;
  *) PROMPT_COMMAND="_agent_monitor_prompt_command;${PROMPT_COMMAND:-}" ;;
esac
