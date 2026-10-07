_agent_monitor_sock="${HOME}/.cache/smuler/agent-monitor.sock"
_agent_monitor_pid=$$

_agent_monitor_send() {
  if [ -S "$_agent_monitor_sock" ]; then
    echo "$1" | nc -U "$_agent_monitor_sock" 2>/dev/null &|
  fi
}

# Escape a string for embedding in a JSON string literal.
_agent_monitor_json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/ }"
  s="${s//$'\t'/ }"
  print -r -- "$s"
}

# Print the agent id for a command line, or nothing. Matches the command word
# exactly so e.g. "ping" or "pip" are not mistaken for "pi".
_agent_monitor_agent_for() {
  case "$1" in
    archer|archer\ *)     print archer ;;
    claude|claude\ *)     print claude ;;
    opencode|opencode\ *) print opencode ;;
    codex|codex\ *)       print codex ;;
    pi|pi\ *)             print pi ;;
    aider|aider\ *)       print aider ;;
  esac
}

_agent_monitor_preexec() {
  local cmd="$1"
  local agent_id="$(_agent_monitor_agent_for "$cmd")"
  _agent_monitor_last_agent="$agent_id"
  if [ -n "$agent_id" ]; then
    local esc="$(_agent_monitor_json_escape "$cmd")"
    _agent_monitor_send "{\"type\":\"agent_start\",\"agentId\":\"$agent_id\",\"agentName\":\"$agent_id\",\"command\":\"$esc\",\"pid\":$$}"
  fi
}

_agent_monitor_precmd() {
  local last_exit=$?
  if [ -n "$_agent_monitor_last_agent" ]; then
    _agent_monitor_send "{\"type\":\"agent_end\",\"agentId\":\"$_agent_monitor_last_agent\",\"exitCode\":$last_exit,\"pid\":$$}"
  fi
  _agent_monitor_last_agent=""
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec _agent_monitor_preexec
add-zsh-hook precmd _agent_monitor_precmd
