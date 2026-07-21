package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lum1n/smuler/plugins/agent-monitor/internal"
	"github.com/lum1n/smuler/plugins/agent-monitor/internal/drift"
	"github.com/lum1n/smuler/plugins/agent-monitor/internal/rules"
	"github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "agent-monitor"
	pluginVersion = "0.1.0"
)

type handler struct {
	store         *internal.AgentStore
	socket        *internal.SocketListener
	cli           *internal.ClaudeHookInstaller
	cxh           *internal.CodexHookInstaller
	procdetect    *internal.ProcDetector
	prevStates    map[string]string
	gcReader      *internal.GenericContextReader
	ccReader      *internal.ClaudeContextReader
	cxReader      *internal.CodexContextReader
	ocReader      *internal.OpenCodeContextReader
	piReader      *internal.PiContextReader
	rulesScanner  *rules.Scanner
	driftAnalyzer *drift.Analyzer
}

func newHandler() *handler {
	store := internal.NewAgentStore()
	return &handler{
		store:         store,
		prevStates:    make(map[string]string),
		cli:           internal.NewClaudeHookInstaller(),
		cxh:           internal.NewCodexHookInstaller(),
		procdetect:    internal.NewProcDetector(store, 5*time.Second),
		gcReader:      internal.NewGenericContextReader(),
		ccReader:      internal.NewClaudeContextReader(),
		cxReader:      internal.NewCodexContextReader(),
		ocReader:      internal.NewOpenCodeContextReader(),
		piReader:      internal.NewPiContextReader(),
		rulesScanner:  rules.NewScanner(),
		driftAnalyzer: drift.NewAnalyzer(),
	}
}

func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
	if strings.HasPrefix(id, "dispatch_task-") {
		pidStr := strings.TrimPrefix(id, "dispatch_task-")
		task := params["task"]
		if task == "" {
			task = params["query"]
		}
		contextStr := params["context"]
		return h.dispatchTask(pidStr, task, contextStr)
	}
	if strings.HasPrefix(id, "handoff_context-") {
		pidStr := strings.TrimPrefix(id, "handoff_context-")
		fromAgent := params["fromAgent"]
		contextStr := params["context"]
		return h.handoffContext(pidStr, fromAgent, contextStr)
	}
	if strings.HasPrefix(id, "open_file-") {
		path := strings.TrimPrefix(id, "open_file-")
		return h.openFile(path)
	}
	if strings.HasPrefix(id, "preview_suggestion-") ||
		strings.HasPrefix(id, "apply_suggestion-") ||
		strings.HasPrefix(id, "dismiss_suggestion-") ||
		strings.HasPrefix(id, "snooze_suggestion-") {
		return h.handleSuggestionAction(id)
	}

	idx := strings.LastIndex(id, "-")
	if idx < 0 {
		return false, "invalid action id: " + id
	}
	actionType := id[:idx]
	pidStr := id[idx+1:]
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return false, "invalid pid in action: " + pidStr
	}

	switch actionType {
	case "stop_agent":
		proc, err := os.FindProcess(pid)
		if err != nil {
			return false, fmt.Sprintf("process %d not found", pid)
		}
		if err := proc.Signal(os.Interrupt); err != nil {
			return false, fmt.Sprintf("failed to signal: %v", err)
		}
		return true, ""
	case "kill_agent":
		exec.Command("kill", "-9", pidStr).Run()
		return true, ""
	case "pause_agent":
		exec.Command("kill", "-STOP", pidStr).Run()
		return true, ""
	case "resume_agent":
		exec.Command("kill", "-CONT", pidStr).Run()
		return true, ""
	case "focus_agent":
		return h.focusAgent(pid)
	case "approve_agent":
		return h.sendInputToAgent(pid, "y")
	case "deny_agent":
		return h.sendInputToAgent(pid, "n")
	default:
		return false, "unknown action: " + actionType
	}
}

func (h *handler) dispatchTask(pidStr, task, contextStr string) (bool, string) {
	if runtime.GOOS != "darwin" {
		return false, "dispatch is only supported on macOS"
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return false, "invalid pid: " + pidStr
	}

	script := fmt.Sprintf(`
tell application "System Events"
	set frontmost of process "Terminal" to true
	delay 0.2
	keystroke "%s"
	keystroke return
end tell
`, escapeAppleScript(task))
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return false, fmt.Sprintf("failed to dispatch task: %v", err)
	}

	sdk.Log("dispatched task to pid %d: %s (context: %s)", pid, task, contextStr)
	sdk.Emit(sdk.Event{
		Type:     "agent_dispatched",
		Severity: sdk.SeverityInfo,
		Message:  fmt.Sprintf("Task dispatched to agent (pid %d): %s", pid, task),
		Data:     map[string]string{"pid": pidStr, "task": task},
	})
	return true, ""
}

func (h *handler) handoffContext(pidStr, fromAgent, contextStr string) (bool, string) {
	if runtime.GOOS != "darwin" {
		return false, "handoff is only supported on macOS"
	}

	handoffMsg := fmt.Sprintf("Context from %s:\n%s", fromAgent, contextStr)
	script := fmt.Sprintf(`
tell application "System Events"
	set frontmost of process "Terminal" to true
	delay 0.2
	keystroke "%s"
	keystroke return
end tell
`, escapeAppleScript(handoffMsg))
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return false, fmt.Sprintf("failed to handoff context: %v", err)
	}

	sdk.Log("handed off context from %s to pid %s", fromAgent, pidStr)
	sdk.Emit(sdk.Event{
		Type:     "agent_handoff",
		Severity: sdk.SeverityInfo,
		Message:  fmt.Sprintf("Context handed off from %s to agent (pid %s)", fromAgent, pidStr),
	})
	return true, ""
}

func (h *handler) focusAgent(pid int) (bool, string) {
	if runtime.GOOS != "darwin" {
		return false, "focus is only supported on macOS"
	}
	script := fmt.Sprintf(`
tell application "System Events"
	set frontmost of process "Terminal" to true
end tell
`)
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return false, fmt.Sprintf("failed to focus Terminal: %v", err)
	}
	return true, ""
}

func (h *handler) sendInputToAgent(pid int, input string) (bool, string) {
	if runtime.GOOS != "darwin" {
		return false, "input injection is only supported on macOS"
	}
	script := fmt.Sprintf(`
tell application "System Events"
	set frontmost of process "Terminal" to true
	keystroke "%s"
	keystroke return
end tell
`, input)
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		return false, fmt.Sprintf("failed to send input: %v", err)
	}
	return true, ""
}

func (h *handler) handleSuggestionAction(id string) (bool, string) {
	prefix := ""
	var suggestionID string
	for _, p := range []string{"preview_suggestion-", "apply_suggestion-", "dismiss_suggestion-", "snooze_suggestion-"} {
		if strings.HasPrefix(id, p) {
			prefix = p
			suggestionID = strings.TrimPrefix(id, p)
			break
		}
	}
	if prefix == "" {
		return false, "unknown suggestion action: " + id
	}

	suggestions := h.driftAnalyzer.Load()
	var suggestion *drift.Suggestion
	for i := range suggestions {
		if suggestions[i].ID == suggestionID {
			suggestion = &suggestions[i]
			break
		}
	}
	if suggestion == nil {
		return false, "suggestion not found: " + suggestionID
	}

	switch prefix {
	case "preview_suggestion-":
		return true, fmt.Sprintf("Proposed change to %s:\n%s", suggestion.ProposedPath, suggestion.ProposedPatch)
	case "apply_suggestion-":
		if err := h.driftAnalyzer.ApplySuggestion(*suggestion); err != nil {
			return false, fmt.Sprintf("failed to apply: %v", err)
		}
		return true, ""
	case "dismiss_suggestion-":
		if err := h.driftAnalyzer.Dismiss(suggestionID); err != nil {
			return false, fmt.Sprintf("failed to dismiss: %v", err)
		}
		return true, ""
	case "snooze_suggestion-":
		until := time.Now().Add(24 * time.Hour).UnixMilli()
		if err := h.driftAnalyzer.Snooze(suggestionID, until); err != nil {
			return false, fmt.Sprintf("failed to snooze: %v", err)
		}
		return true, ""
	}
	return false, "unknown suggestion action: " + id
}

func (h *handler) openFile(path string) (bool, string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Sprintf("failed to open %s: %v", path, err)
	}
	return true, ""
}

func (h *handler) Initialize(params sdk.InitializeParams) string {
	paths := sdk.HostPathsFromConfig(params.Config)
	socketPath := filepath.Join(paths.CacheDir, "agent-monitor.sock")

	if h.socket != nil {
		h.socket.Stop()
	}
	h.socket = internal.NewSocketListener(socketPath, h.store)

	if err := h.socket.Start(); err != nil {
		sdk.Log("socket start failed: %v", err)
		return sdk.HealthDegraded
	}
	h.procdetect.Start()
	if err := h.cli.Install(); err != nil {
		sdk.Log("claude hook install failed: %v", err)
	}
	if err := h.cxh.Install(); err != nil {
		sdk.Log("codex hook install failed: %v", err)
	}
	return sdk.HealthOK
}

func (h *handler) GetStatus() sdk.Snapshot {
	agents := h.enrichSessions(h.store.List())
	history := h.store.History()

	active := 0
	question := 0
	errors := 0
	completed := 0

	items := make([]sdk.Item, 0, len(agents)+len(history)+1)
	for _, a := range agents {
		severity := sdk.SeverityInfo
		subtitle := agentDisplayStatus(a.State)
		ctx := h.renderContext(a)
		prev := h.prevStates[a.ID]

		displayName := a.DisplayName
		if displayName == "" {
			displayName = strings.Title(a.AgentID)
		}

		if prev != a.State {
			h.prevStates[a.ID] = a.State
			switch a.State {
			case "completed":
				sdk.Emit(sdk.Event{
					Type:     "agent_completed",
					Severity: sdk.SeverityInfo,
					Message:  fmt.Sprintf("%s finished: %s", displayName, a.Task),
					Data:     map[string]string{"agentId": a.ID},
				})
			case "error":
				sdk.Emit(sdk.Event{
					Type:     "agent_error",
					Severity: sdk.SeverityCritical,
					Message:  fmt.Sprintf("%s failed: %s", displayName, a.Task),
					Data:     map[string]string{"agentId": a.ID},
				})
			case "question":
				sdk.Emit(sdk.Event{
					Type:     "agent_question",
					Severity: sdk.SeverityWarning,
					Message:  fmt.Sprintf("%s needs your attention: %s", displayName, a.Task),
					Data:     map[string]string{"agentId": a.ID},
				})
			}
		}

		switch a.State {
		case "running", "idle":
			// process exists but idle/waiting for input
		case "working", "thinking":
			active++
		case "question":
			question++
			severity = sdk.SeverityWarning
			subtitle = "needs input"
		case "error":
			errors++
			severity = sdk.SeverityCritical
		case "completed":
			completed++
			subtitle = "done"
		case "paused":
			subtitle = "paused"
		}

		subtitleParts := []string{displayName, subtitle}
		if ctx.branch != "" {
			subtitleParts = append(subtitleParts, ctx.branch)
		}
		if ctx.model != "" {
			subtitleParts = append(subtitleParts, ctx.model)
		}
		if ctx.currentTool != "" {
			subtitleParts = append(subtitleParts, ctx.currentTool)
		}

		detailParts := []string{}
		if a.CurrentFile != "" {
			detailParts = append(detailParts, a.CurrentFile)
		}
		if a.Task != "" {
			detailParts = append(detailParts, a.Task)
		}
		if a.Provider != "" && a.Cost > 0 {
			detailParts = append(detailParts, fmt.Sprintf("%s · $%.4f", a.Provider, a.Cost))
		} else if a.Provider != "" {
			detailParts = append(detailParts, a.Provider)
		}
		if ctx.repoFullName != "" {
			detailParts = append(detailParts, ctx.repoFullName)
		}
		detail := strings.Join(detailParts, "\n")
		if detail == "" {
			detail = a.Command
		}

		title := displayName
		if ctx.repoName != "" {
			title = fmt.Sprintf("%s · %s", displayName, ctx.repoName)
		}
		if a.PID > 0 {
			title = fmt.Sprintf("%s (%d)", title, a.PID)
		}

		actions := []sdk.Action{}
		if a.PID > 0 && (a.State == "running" || a.State == "question") {
			pidStr := strconv.Itoa(a.PID)
			actions = append(actions,
				sdk.Action{ID: "stop_agent-" + pidStr, Label: "Stop"},
				sdk.Action{ID: "pause_agent-" + pidStr, Label: "Pause"})
		}
		if a.PID > 0 && a.State == "question" {
			pidStr := strconv.Itoa(a.PID)
			actions = append(actions,
				sdk.Action{ID: "focus_agent-" + pidStr, Label: "Focus"},
				sdk.Action{ID: "approve_agent-" + pidStr, Label: "Approve"},
				sdk.Action{ID: "deny_agent-" + pidStr, Label: "Deny"})
		}
		if a.PID > 0 && a.State == "paused" {
			pidStr := strconv.Itoa(a.PID)
			actions = append(actions,
				sdk.Action{ID: "resume_agent-" + pidStr, Label: "Resume"},
				sdk.Action{ID: "kill_agent-" + pidStr, Label: "Kill"})
		}

		meta := agentMetadata(a)
		if insight := internal.ParseOutput(a.OutputTail); insight.HasBlocker || insight.HasErrorLoop || insight.IsComplete {
			if insight.HasBlocker {
				meta["blocker"] = insight.BlockerKind
			}
			if insight.HasErrorLoop {
				meta["errorLoop"] = "true"
			}
			if insight.IsComplete {
				meta["taskComplete"] = "true"
			}
		}

		items = append(items, sdk.Item{
			ID:        a.ID,
			Title:     title,
			Subtitle:  strings.Join(subtitleParts, " · "),
			Detail:    detail,
			Severity:  severity,
			Timestamp: time.UnixMilli(a.UpdatedAt).UTC().Format(time.RFC3339),
			Actions:   actions,
			Metadata:  meta,
		})
	}

	alive := make(map[string]bool, len(agents))
	for _, a := range agents {
		alive[a.ID] = true
	}
	for id := range h.prevStates {
		if !alive[id] {
			delete(h.prevStates, id)
		}
	}

	summaryValue := fmt.Sprintf("%d agents", len(agents))
	severity := sdk.SeverityInfo
	if question > 0 {
		summaryValue = fmt.Sprintf("⚠ %d need input", question)
		severity = sdk.SeverityWarning
	} else if errors > 0 {
		severity = sdk.SeverityCritical
	} else if active > 0 {
		summaryValue = fmt.Sprintf("%d active", active)
	}

	alerts := []sdk.Alert{}
	if question > 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "attention",
			Severity: sdk.SeverityWarning,
			Message:  fmt.Sprintf("%d agent(s) need your input", question),
		})
	}

	dispatchActions := make([]sdk.Action, 0, len(agents))
	for _, a := range agents {
		if a.PID <= 0 {
			continue
		}
		switch a.State {
		case "completed", "error":
			continue
		}
		displayName := a.DisplayName
		if displayName == "" {
			displayName = strings.Title(a.AgentID)
		}
		pidStr := strconv.Itoa(a.PID)
		dispatchActions = append(dispatchActions, sdk.Action{
			ID:    "dispatch_task-" + pidStr,
			Label: "Dispatch to " + displayName + " (pid " + pidStr + ")",
		})
	}

	setup := h.setupItems(agents)
	items = append(items, setup...)

	suggestions := h.suggestionItems(agents, setup)
	items = append(items, suggestions...)

	if len(agents) == 0 {
		items = append(items, sdk.Item{
			ID:       "waiting",
			Title:    "No agents detected",
			Subtitle: "Run archer, claude, codex, opencode, or pi in your terminal",
			Severity: sdk.SeverityInfo,
			Actions:  []sdk.Action{},
		})
	}

	if len(history) > 0 {
		items = append(items, sdk.Item{
			ID:       "history-header",
			Title:    "Recent History",
			Subtitle: fmt.Sprintf("%d entries", len(history)),
			Severity: sdk.SeverityInfo,
			Actions:  []sdk.Action{},
		})
		for _, hist := range history {
			items = append(items, sdk.Item{
				ID:        "history-" + hist.ID,
				Title:     fmt.Sprintf("%s: %s", hist.AgentID, hist.State),
				Subtitle:  hist.Command,
				Detail:    hist.Task,
				Severity:  sdk.SeverityInfo,
				Timestamp: time.UnixMilli(hist.Timestamp).UTC().Format(time.RFC3339),
				Actions:   []sdk.Action{},
			})
		}
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateReady,
		Summary: sdk.Summary{
			Title:    "Agents",
			Value:    summaryValue,
			Trend:    sdk.TrendSteady,
			Severity: severity,
			IconHint: "cpu",
		},
		Items:        items,
		Actions:      dispatchActions,
		Alerts:       alerts,
		RefreshAfter: 5,
		Health:       sdk.HealthOK,
	}
}

func (h *handler) setupItems(agents []internal.AgentSession) []sdk.Item {
	repoRoots := make(map[string]bool)
	for _, a := range agents {
		root := a.RepoRoot
		if root == "" && a.CWD != "" {
			root = a.CWD
		}
		if root != "" {
			repoRoots[root] = true
		}
	}

	roots := make([]string, 0, len(repoRoots))
	for r := range repoRoots {
		roots = append(roots, r)
	}

	files := h.rulesScanner.Scan(roots)
	if len(files) == 0 {
		return nil
	}

	items := make([]sdk.Item, 0, len(files)+1)
	items = append(items, sdk.Item{
		ID:       "setup-header",
		Title:    "Setup",
		Subtitle: fmt.Sprintf("%d rule/config files", len(files)),
		Severity: sdk.SeverityInfo,
		Actions:  []sdk.Action{},
	})

	for _, f := range files {
		detail := f.Excerpt
		if detail == "" {
			detail = f.Path
		}
		items = append(items, sdk.Item{
			ID:       "setup-" + f.RepoName + "-" + f.Kind.String() + "-" + filepath.Base(f.Path),
			Title:    f.Title,
			Subtitle: f.Kind.DisplayName() + " · " + f.RepoName,
			Detail:   detail,
			Severity: sdk.SeverityInfo,
			Actions: []sdk.Action{{
				ID:    "open_file-" + f.Path,
				Label: "Open",
			}},
			Metadata: map[string]string{
				"filePath": f.Path,
				"repoName": f.RepoName,
				"ruleKind": string(f.Kind),
			},
		})
	}

	missing := h.missingRules(roots, files)
	for _, m := range missing {
		items = append(items, sdk.Item{
			ID:       "setup-missing-" + m.repo + "-" + m.kind,
			Title:    "Missing " + m.kind,
			Subtitle: m.repo,
			Detail:   m.hint,
			Severity: sdk.SeverityWarning,
			Actions:  []sdk.Action{},
			Metadata: map[string]string{
				"repoName": m.repo,
				"missing":  "true",
				"ruleKind": m.kind,
			},
		})
	}

	return items
}

type missingRule struct {
	repo string
	kind string
	hint string
}

func (h *handler) missingRules(roots []string, files []rules.RuleFile) []missingRule {
	has := make(map[string]bool)
	for _, f := range files {
		if f.Kind == rules.KindAgentsMd {
			has[f.RepoRoot] = true
		}
	}

	var out []missingRule
	for _, root := range roots {
		if !has[root] {
			out = append(out, missingRule{
				repo: filepath.Base(root),
				kind: "AGENTS.md",
				hint: "Add AGENTS.md to share project instructions with all agents.",
			})
		}
	}
	return out
}

func (h *handler) suggestionItems(agents []internal.AgentSession, setup []sdk.Item) []sdk.Item {
	repoRules := make(map[string][]string)
	repoSessions := make(map[string][]drift.Session)

	for _, item := range setup {
		if item.Metadata == nil {
			continue
		}
		repo := item.Metadata["repoName"]
		path := item.Metadata["filePath"]
		if repo == "" || path == "" {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		repoRules[repo] = append(repoRules[repo], string(content))
	}

	for _, a := range agents {
		repo := a.RepoName
		if repo == "" {
			repo = filepath.Base(a.CWD)
		}
		if repo == "" || repo == "." {
			continue
		}
		repoSessions[repo] = append(repoSessions[repo], drift.Session{
			ID:   a.ID,
			Task: a.Task,
		})
	}

	var all []drift.Suggestion
	for repo, sessions := range repoSessions {
		all = append(all, h.driftAnalyzer.Analyze(repo, repoRules[repo], sessions)...)
	}

	if len(all) == 0 {
		return nil
	}

	items := make([]sdk.Item, 0, len(all)+1)
	items = append(items, sdk.Item{
		ID:       "suggestions-header",
		Title:    "Suggestions",
		Subtitle: fmt.Sprintf("%d detected", len(all)),
		Severity: sdk.SeverityInfo,
		Actions:  []sdk.Action{},
	})

	for _, s := range all {
		severity := sdk.SeverityInfo
		switch s.Severity {
		case drift.SeverityWarning:
			severity = sdk.SeverityWarning
		case drift.SeverityCritical:
			severity = sdk.SeverityCritical
		}

		items = append(items, sdk.Item{
			ID:       "suggestion-" + s.ID,
			Title:    s.Title,
			Subtitle: string(s.Kind) + " · " + s.Repo,
			Detail:   s.Description,
			Severity: severity,
			Actions: []sdk.Action{
				{ID: "preview_suggestion-" + s.ID, Label: "Preview"},
				{ID: "apply_suggestion-" + s.ID, Label: "Apply"},
				{ID: "dismiss_suggestion-" + s.ID, Label: "Dismiss"},
				{ID: "snooze_suggestion-" + s.ID, Label: "Snooze"},
			},
			Metadata: map[string]string{
				"repoName":      s.Repo,
				"suggestionId":  s.ID,
				"kind":          string(s.Kind),
				"proposedPath":  s.ProposedPath,
				"proposedPatch": s.ProposedPatch,
				"evidence":      strings.Join(s.Evidence, "\n"),
			},
		})
	}

	return items
}

func (h *handler) enrichSessions(sessions []internal.AgentSession) []internal.AgentSession {
	out := make([]internal.AgentSession, len(sessions))
	for i, a := range sessions {
		out[i] = a.Clone()
		if a.PID <= 0 {
			continue
		}

		gc := h.gcReader.ContextForPID(a.PID)
		if gc.Available {
			if out[i].CWD == "" {
				out[i].CWD = gc.CWD
			}
			if out[i].RepoName == "" {
				out[i].RepoName = gc.RepoName
			}
			if out[i].RepoFullName == "" {
				out[i].RepoFullName = gc.RepoFullName
			}
			if out[i].Branch == "" {
				out[i].Branch = gc.Branch
			}
			if !out[i].IsRepoDirty {
				out[i].IsRepoDirty = gc.IsRepoDirty
			}
			if out[i].FilesChanged == 0 {
				out[i].FilesChanged = gc.FilesChanged
			}
		}

		var ctx internal.AgentSession
		switch a.AgentID {
		case "opencode":
			ctx = h.ocReader.ContextForPID(a.PID)
		case "claude":
			ctx = h.ccReader.ContextForPID(a.PID)
		case "codex":
			ctx = h.cxReader.ContextForPID(a.PID)
		case "pi":
			ctx = h.piReader.ContextForPID(a.PID)
		}

		if ctx.Available {
			mergeSessionContext(&out[i], ctx)
		}
	}
	return out
}

func mergeSessionContext(dst *internal.AgentSession, src internal.AgentSession) {
	if src.DisplayName != "" {
		dst.DisplayName = src.DisplayName
	}
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.Task != "" {
		dst.Task = src.Task
	}
	if src.CurrentFile != "" {
		dst.CurrentFile = src.CurrentFile
	}
	if src.CurrentTool != "" {
		dst.CurrentTool = src.CurrentTool
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	if src.Provider != "" {
		dst.Provider = src.Provider
	}
	if src.Cost != 0 {
		dst.Cost = src.Cost
	}
	if src.TokensInput != 0 {
		dst.TokensInput = src.TokensInput
	}
	if src.TokensOutput != 0 {
		dst.TokensOutput = src.TokensOutput
	}
	if src.CacheRead != 0 {
		dst.CacheRead = src.CacheRead
	}
	if src.CacheWrite != 0 {
		dst.CacheWrite = src.CacheWrite
	}
	if src.FilesChanged != 0 {
		dst.FilesChanged = src.FilesChanged
	}
	if src.Additions != 0 {
		dst.Additions = src.Additions
	}
	if src.Deletions != 0 {
		dst.Deletions = src.Deletions
	}
	if src.CWD != "" {
		dst.CWD = src.CWD
	}
	if src.RepoName != "" {
		dst.RepoName = src.RepoName
	}
	if src.RepoFullName != "" {
		dst.RepoFullName = src.RepoFullName
	}
	if src.Branch != "" {
		dst.Branch = src.Branch
	}
	if src.IsRepoDirty {
		dst.IsRepoDirty = src.IsRepoDirty
	}
	if len(src.Todos) > 0 {
		dst.Todos = src.Todos
	}
	if src.State != "" {
		dst.State = internal.MergeAgentState(dst.State, src.State)
	}
	if src.QuestionText != "" {
		dst.QuestionText = src.QuestionText
	}
}

type renderedContext struct {
	repoName     string
	repoFullName string
	branch       string
	model        string
	currentTool  string
}

func (h *handler) renderContext(a internal.AgentSession) renderedContext {
	ctx := renderedContext{}
	ctx.repoName = a.RepoName
	ctx.repoFullName = a.RepoFullName
	ctx.branch = a.Branch
	ctx.model = a.Model
	ctx.currentTool = a.CurrentTool
	return ctx
}

func agentMetadata(a internal.AgentSession) map[string]string {
	m := map[string]string{
		"agentId":   a.AgentID,
		"sessionId": a.SessionID,
		"cwd":       a.CWD,
		"state":     a.State,
	}
	if a.PID > 0 {
		m["pid"] = strconv.Itoa(a.PID)
	}
	if a.Task != "" {
		m["currentTask"] = a.Task
	}
	if a.RepoName != "" {
		m["repoName"] = a.RepoName
	}
	if a.RepoFullName != "" {
		m["repoFullName"] = a.RepoFullName
	}
	if a.Branch != "" {
		m["branch"] = a.Branch
	}
	if a.CurrentFile != "" {
		m["currentFile"] = a.CurrentFile
	}
	if a.CurrentTool != "" {
		m["currentTool"] = a.CurrentTool
	}
	if a.Model != "" {
		m["model"] = a.Model
	}
	if a.Provider != "" {
		m["provider"] = a.Provider
	}
	if a.Cost != 0 {
		m["cost"] = fmt.Sprintf("%.6f", a.Cost)
	}
	if a.TokensInput != 0 {
		m["tokensInput"] = strconv.FormatInt(a.TokensInput, 10)
	}
	if a.TokensOutput != 0 {
		m["tokensOutput"] = strconv.FormatInt(a.TokensOutput, 10)
	}
	if a.IsRepoDirty {
		m["isRepoDirty"] = "true"
	}
	if a.QuestionText != "" {
		m["questionText"] = a.QuestionText
	}
	return m
}

func agentDisplayStatus(state string) string {
	switch state {
	case "running":
		return "idle"
	case "working":
		return "working"
	case "thinking":
		return "thinking"
	case "question":
		return "needs input"
	case "completed":
		return "completed"
	case "error":
		return "error"
	case "paused":
		return "paused"
	default:
		return state
	}
}

func (h *handler) Shutdown() {
	if h.socket != nil {
		h.socket.Stop()
	}
	h.cli.Uninstall()
	h.cxh.Uninstall()
	h.procdetect.Stop()
}

func main() {
	sdk.Run(pluginID, pluginVersion, newHandler())
}

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	s = strings.ReplaceAll(s, "'", "'\\''")
	return s
}

