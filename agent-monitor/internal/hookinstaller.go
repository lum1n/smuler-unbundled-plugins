package internal

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HookEventConfig describes one native hook event to register.
type HookEventConfig struct {
	Name    string
	Matcher string // "*" for matched events, "" for unmatched
}

// NativeHookConfig configures a native hook installer for a specific agent.
type NativeHookConfig struct {
	AgentID       string
	AgentName     string
	ScriptName    string
	SettingsPath  string
	BackupSuffix  string
	Events        []HookEventConfig
	ExtraToolLabels map[string]string
}

// NativeHookInstaller writes a small Node script and registers it as native
// hooks in the agent's settings file. It is intentionally additive: it only
// modifies the "hooks" section and skips writing entirely when the hooks are
// already present.
type NativeHookInstaller struct {
	cfg        NativeHookConfig
	scriptPath string
}

func NewNativeHookInstaller(cfg NativeHookConfig) *NativeHookInstaller {
	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".cache", "smuler")
	return &NativeHookInstaller{
		cfg:        cfg,
		scriptPath: filepath.Join(cacheDir, cfg.ScriptName),
	}
}

func (i *NativeHookInstaller) ScriptPath() string { return i.scriptPath }

func (i *NativeHookInstaller) Install() error {
	cacheDir := filepath.Dir(i.scriptPath)
	if err := os.MkdirAll(cacheDir, 0700); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}
	if err := os.WriteFile(i.scriptPath, []byte(i.hookScript()), 0700); err != nil {
		return fmt.Errorf("write hook script: %w", err)
	}

	nodePath, err := exec.LookPath("node")
	if err != nil {
		log.Printf("[agent-monitor] node not found, skipping %s hook install: %v", i.cfg.AgentName, err)
		return nil
	}

	settingsPath := i.cfg.SettingsPath
	settings := i.loadSettings(settingsPath)

	if i.isInstalled(settings, nodePath) {
		log.Printf("[agent-monitor] %s hooks already installed in %s", i.cfg.AgentName, settingsPath)
		return nil
	}

	if err := i.backup(settingsPath); err != nil {
		log.Printf("[agent-monitor] warning: failed to backup %s: %v", settingsPath, err)
	}

	i.apply(settings, nodePath)

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(settingsPath, data, 0600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}

	log.Printf("[agent-monitor] installed %s native hooks in %s", i.cfg.AgentName, settingsPath)
	return nil
}

func (i *NativeHookInstaller) Uninstall() {
	os.Remove(i.scriptPath)

	settingsPath := i.cfg.SettingsPath
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return
	}

	settings := map[string]any{}
	if json.Unmarshal(data, &settings) != nil {
		return
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return
	}

	for key, val := range hooks {
		arr, _ := val.([]any)
		var out []any
		for _, item := range arr {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			hooksList, _ := entry["hooks"].([]any)
			var clean []any
			for _, h := range hooksList {
				hm, _ := h.(map[string]any)
				if hm == nil {
					continue
				}
				cmdStr, _ := hm["command"].(string)
				if strings.Contains(cmdStr, i.scriptPath) {
					continue
				}
				clean = append(clean, h)
			}
			if len(clean) > 0 {
				entry["hooks"] = clean
				out = append(out, entry)
			}
		}
		if len(out) == 0 {
			delete(hooks, key)
		} else {
			hooks[key] = out
		}
	}

	out, _ := json.MarshalIndent(settings, "", "  ")
	out = append(out, '\n')
	os.WriteFile(settingsPath, out, 0600)
	log.Printf("[agent-monitor] removed %s native hooks from %s", i.cfg.AgentName, settingsPath)
}

func (i *NativeHookInstaller) loadSettings(path string) map[string]any {
	settings := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &settings)
	}
	return settings
}

func (i *NativeHookInstaller) backup(settingsPath string) error {
	bak := settingsPath + i.cfg.BackupSuffix
	if _, err := os.Stat(bak); err == nil {
		return nil
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	return os.WriteFile(bak, data, 0600)
}

func (i *NativeHookInstaller) isInstalled(settings map[string]any, nodePath string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}

	for _, evt := range i.cfg.Events {
		arr, _ := hooks[evt.Name].([]any)
		if !i.eventHasHook(arr, evt, nodePath) {
			return false
		}
	}
	return true
}

func (i *NativeHookInstaller) eventHasHook(arr []any, evt HookEventConfig, nodePath string) bool {
	expectedCmd := nodePath + " " + i.scriptPath + " " + evt.Name
	for _, item := range arr {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if evt.Matcher != "" {
			if m, _ := entry["matcher"].(string); m != evt.Matcher {
				continue
			}
		}
		hooksList, _ := entry["hooks"].([]any)
		for _, h := range hooksList {
			hm, _ := h.(map[string]any)
			if hm == nil {
				continue
			}
			cmdStr, _ := hm["command"].(string)
			if cmdStr == expectedCmd {
				return true
			}
		}
	}
	return false
}

func (i *NativeHookInstaller) apply(settings map[string]any, nodePath string) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}

	cmd := func(evt string) string { return nodePath + " " + i.scriptPath + " " + evt }

	stripOurs := func(arr []any) []any {
		var out []any
		for _, item := range arr {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			hooksList, _ := entry["hooks"].([]any)
			var clean []any
			for _, h := range hooksList {
				hm, _ := h.(map[string]any)
				if hm == nil {
					continue
				}
				cmdStr, _ := hm["command"].(string)
				if strings.Contains(cmdStr, i.scriptPath) {
					continue
				}
				clean = append(clean, h)
			}
			entry["hooks"] = clean
			if len(clean) > 0 {
				out = append(out, entry)
			}
		}
		return out
	}

	for _, evt := range i.cfg.Events {
		arr, _ := hooks[evt.Name].([]any)
		arr = stripOurs(arr)
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd(evt.Name)}}}
		if evt.Matcher != "" {
			entry["matcher"] = evt.Matcher
		}
		arr = append(arr, entry)
		hooks[evt.Name] = arr
	}
}

func (i *NativeHookInstaller) hookScript() string {
	labels := map[string]string{
		"Bash":         "Running command",
		"Edit":         "Editing",
		"Write":        "Writing",
		"Read":         "Reading",
		"Grep":         "Searching",
		"Glob":         "Searching",
		"WebFetch":     "Browsing web",
		"WebSearch":    "Searching web",
		"Task":         "Delegating",
		"TodoWrite":    "Planning",
		"NotebookEdit": "Editing",
		"MultiEdit":    "Editing",
	}
	for k, v := range i.cfg.ExtraToolLabels {
		labels[k] = v
	}
	labelJSON, _ := json.Marshal(labels)

	return fmt.Sprintf(`#!/usr/bin/env node
const net = require("net");
const os = require("os");
const path = require("path");
const SOCKET = path.join(os.homedir(), ".cache", "smuler", "agent-monitor.sock");
const rawEvent = process.argv[2] || "";
const LABELS = %s;
const EVENT_ALIASES = {
  UserPromptSubmit: "prompt",
  PreToolUse: "pre",
  PostToolUse: "post",
  PermissionRequest: "permreq",
  Stop: "stop",
};
const event = EVENT_ALIASES[rawEvent] || rawEvent;
let raw = "";
process.stdin.on("data", d => raw += d);
process.stdin.on("end", () => {
  let p = {};
  try { p = JSON.parse(raw || "{}"); } catch {}
  let state = "running", label = "", msg = "";
  let questionText = "";
  switch (event) {
    case "prompt": state = "thinking"; label = "Thinking…"; break;
    case "pre": state = "working"; label = LABELS[p.tool_name] || "Using tool"; break;
    case "post": state = "thinking"; label = "Thinking…"; break;
    case "permreq": state = "question"; label = "needs permission"; msg = "Awaiting permission"; questionText = (p.message || p.text || "").slice(0, 500); break;
    case "stop": state = "completed"; label = "Done"; break;
    default: return;
  }
  const payload = JSON.stringify({type:"agent_status",agentId:"%s",agentName:"%s",pid:process.ppid,state,label,message:msg,questionText,tool:p.tool_name||"",sessionId:(p.session_id||"").replace(/[^A-Za-z0-9_.-]/g,"").slice(0,64),cwd:p.cwd||""});
  const sock = net.createConnection(SOCKET, () => sock.end(payload + "\n"));
  sock.on("error", (err) => { console.error("[smuler-hook] socket error:", err.message); });
  setTimeout(() => sock.destroy(), 2000);
});
`, string(labelJSON), i.cfg.AgentID, i.cfg.AgentName)
}
