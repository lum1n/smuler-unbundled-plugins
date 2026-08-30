package internal

import (
	"time"

	"github.com/lum1n/smuler/plugins/agent-monitor/internal/watcher"
)

// PaneLookuper resolves pane PID/cwd for a tmux session:window.
type PaneLookuper func(session string, window int) (pid int, cwd string, err error)

// IngestWatcherEvent applies one agent-watcher NDJSON event to the store.
func IngestWatcherEvent(store *AgentStore, ev watcher.Event, lookup PaneLookuper) {
	switch ev.Type {
	case "snapshot":
		keep := make(map[string]bool, len(ev.Agents))
		for _, info := range ev.Agents {
			id := applyWatcherAgent(store, info, lookup)
			if id != "" {
				keep[id] = true
			}
		}
		store.RetainWatcherIDs(keep)
	case "state":
		applyWatcherAgent(store, watcher.AgentInfo{
			Session:    ev.Session,
			Window:     ev.Window,
			Kind:       ev.Kind,
			State:      ev.State,
			Path:       ev.Path,
			SessionID:  ev.SessionID,
			ToolName:   ev.ToolName,
			ToolTarget: ev.ToolTarget,
			Summary:    ev.Summary,
			Attached:   ev.Attached,
			Windows:    ev.Windows,
			Unbound:    ev.Unbound,
		}, lookup)
	case "unbound":
		applyWatcherAgent(store, watcher.AgentInfo{
			Session:  ev.Session,
			Window:   ev.Window,
			Kind:     ev.Kind,
			Unbound:  true,
			Attached: ev.Attached,
			Windows:  ev.Windows,
		}, lookup)
	case "gone":
		id := watcher.SessionKey(ev.Session, ev.Window)
		existing := store.Get(id)
		if existing == nil {
			return
		}
		store.AddHistory(existing.AgentID, existing.Command, "completed", existing.Task, time.Now().UnixMilli())
		store.Upsert(AgentSession{
			ID:     id,
			State:  "completed",
			Source: SourceTmux,
		})
	}
}

func applyWatcherAgent(store *AgentStore, info watcher.AgentInfo, lookup PaneLookuper) string {
	if info.Session == "" && info.Kind == "" {
		return ""
	}
	id := watcher.SessionKey(info.Session, info.Window)
	mapped := watcher.MapState(info.State, info.Unbound)
	sess := AgentSession{
		ID:          id,
		AgentID:     info.Kind,
		DisplayName: watcher.DisplayName(info.Kind),
		State:       mapped,
		SessionID:   info.SessionID,
		CurrentTool: info.ToolName,
		Task:        info.Summary,
		Source:      SourceTmux,
		TmuxSession: info.Session,
		TmuxWindow:  info.Window,
		WatcherPath: info.Path,
		Unbound:     info.Unbound,
	}
	if mapped == "question" && info.Summary != "" {
		sess.QuestionText = info.Summary
	}
	if lookup != nil {
		if pid, cwd, err := lookup(info.Session, info.Window); err == nil {
			sess.PID = pid
			sess.CWD = cwd
		}
	}
	store.Upsert(sess)
	return id
}
