# smuler Agent Monitor — fish hook
# Installed automatically by smuler.

set -g _agent_monitor_sock "$HOME/.cache/smuler/agent-monitor.sock"

function __agent_monitor_send
    if test -S "$_agent_monitor_sock"
        echo $argv[1] | nc -U "$_agent_monitor_sock" 2>/dev/null &
        disown 2>/dev/null
    end
end

# Print the agent id for a command line, or nothing. Matches the command word
# exactly so e.g. "ping" or "pip" are not mistaken for "pi".
function __agent_monitor_agent_for
    set -l word (string split -m1 " " -- $argv[1])[1]
    switch "$word"
        case archer claude opencode codex pi aider
            echo $word
    end
end

function __agent_monitor_preexec --on-event fish_preexec
    set -g _agent_monitor_agent (__agent_monitor_agent_for "$argv[1]")
    if test -n "$_agent_monitor_agent"
        set -l esc (string replace -a '\\' '\\\\' -- "$argv[1]" | string replace -a '"' '\\"' | string join ' ')
        __agent_monitor_send '{"type":"agent_start","agentId":"'$_agent_monitor_agent'","agentName":"'$_agent_monitor_agent'","command":"'$esc'","pid":'$fish_pid'}'
    end
end

function __agent_monitor_postexec --on-event fish_postexec
    set -l ec $status
    if test -n "$_agent_monitor_agent"
        __agent_monitor_send '{"type":"agent_end","agentId":"'$_agent_monitor_agent'","exitCode":'$ec',"pid":'$fish_pid'}'
    end
    set -g _agent_monitor_agent ""
end
