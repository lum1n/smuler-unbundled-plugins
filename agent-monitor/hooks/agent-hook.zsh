_agent_monitor_sock="${HOME}/.cache/smuler/agent-monitor.sock"
_agent_monitor_pid=$$

_agent_monitor_send() {
  if [ -S "$_agent_monitor_sock" ]; then
    echo "$1" | nc -U "$_agent_monitor_sock" 2>/dev/null &|
  fi
}

_agent_monitor_known_agents="archer|claude|opencode|codex|pi|aider"

_agent_monitor_preexec() {
  local cmd="$1"
  local agent_id=""
  case "$cmd" in
    archer*)   agent_id="archer" ;;
    claude*)   agent_id="claude" ;;
    opencode*) agent_id="opencode" ;;
    codex*)    agent_id="codex" ;;
    pi*)       agent_id="pi" ;;
    aider*)    agent_id="aider" ;;
  esac
  if [ -n "$agent_id" ]; then
    _agent_monitor_send "{\"type\":\"agent_start\",\"agentId\":\"$agent_id\",\"agentName\":\"$agent_id\",\"command\":\"$cmd\",\"pid\":$$}"
  fi
}

_agent_monitor_precmd() {
  local last_exit=$?
  local last_cmd=""
  if [ -n "$_agent_monitor_last_cmd" ]; then
    case "$_agent_monitor_last_cmd" in
      archer*|claude*|opencode*|codex*|pi*|aider*)
        local agent_id="${_agent_monitor_last_cmd%% *}"
        agent_id="${agent_id%%/*}"
        _agent_monitor_send "{\"type\":\"agent_end\",\"agentId\":\"$agent_id\",\"exitCode\":$last_exit,\"pid\":$$}"
        ;;
    esac
  fi
  _agent_monitor_last_cmd="$1"
}

autoload -Uz add-zsh-hook
add-zsh-hook preexec _agent_monitor_preexec
add-zsh-hook precmd _agent_monitor_precmd
