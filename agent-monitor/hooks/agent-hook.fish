# smuler Agent Monitor — fish hook
# Installed automatically by smuler.

set -g _agent_monitor_sock "$HOME/.cache/smuler/agent-monitor.sock"

function __agent_monitor_send
    if test -S "$_agent_monitor_sock"
        echo $argv[1] | nc -U "$_agent_monitor_sock" 2>/dev/null &
    end
end

function fish_preexec --on-event fish_preexec
    set -g _agent_monitor_cmd $argv[1]
    set -l agent_id ""
    switch "$argv[1]"
        case 'archer*'
            set agent_id archer
        case 'claude*'
            set agent_id claude
        case 'opencode*'
            set agent_id opencode
        case 'codex*'
            set agent_id codex
        case 'pi*'
            set agent_id pi
        case 'aider*'
            set agent_id aider
    end
    if test -n "$agent_id"
        __agent_monitor_send '{"type":"agent_start","agentId":"'$agent_id'","agentName":"'$agent_id'","command":"'$argv[1]'","pid":'%self'}
    end
end

function fish_postexec --on-event fish_postexec
    set -l ec $status
    if set -q _agent_monitor_cmd
        switch "$_agent_monitor_cmd"
            case 'archer*' 'claude*' 'opencode*' 'codex*' 'pi*' 'aider*'
                set -l agent_id (string split " " $_agent_monitor_cmd)[1]
                __agent_monitor_send '{"type":"agent_end","agentId":"'$agent_id'","exitCode":'$ec',"pid":%self}'
        end
        set -e _agent_monitor_cmd
    end
end
