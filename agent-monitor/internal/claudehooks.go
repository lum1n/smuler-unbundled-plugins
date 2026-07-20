package internal

import (
	"os"
	"path/filepath"
)

type ClaudeHookInstaller struct {
	inner *NativeHookInstaller
}

func NewClaudeHookInstaller() *ClaudeHookInstaller {
	home, _ := os.UserHomeDir()
	return &ClaudeHookInstaller{
		inner: NewNativeHookInstaller(NativeHookConfig{
			AgentID:      "claude",
			AgentName:    "Claude Code",
			ScriptName:   "claude-hook.js",
			SettingsPath: filepath.Join(home, ".claude", "settings.json"),
			BackupSuffix: ".bak-smuler",
			Events: []HookEventConfig{
				{Name: "UserPromptSubmit"},
				{Name: "PreToolUse", Matcher: "*"},
				{Name: "PostToolUse", Matcher: "*"},
				{Name: "PermissionRequest", Matcher: "*"},
				{Name: "Stop"},
			},
		}),
	}
}

func (i *ClaudeHookInstaller) Install() error { return i.inner.Install() }
func (i *ClaudeHookInstaller) Uninstall()     { i.inner.Uninstall() }
