package internal

import (
	"os"
	"path/filepath"
)

type CodexHookInstaller struct {
	inner *NativeHookInstaller
}

func NewCodexHookInstaller() *CodexHookInstaller {
	home, _ := os.UserHomeDir()
	return &CodexHookInstaller{
		inner: NewNativeHookInstaller(NativeHookConfig{
			AgentID:      "codex",
			AgentName:    "Codex",
			ScriptName:   "codex-hook.js",
			SettingsPath: filepath.Join(home, ".codex", "hooks.json"),
			BackupSuffix: ".bak-smuler",
			Events: []HookEventConfig{
				{Name: "UserPromptSubmit"},
				{Name: "PreToolUse", Matcher: "*"},
				{Name: "PostToolUse", Matcher: "*"},
				{Name: "PermissionRequest", Matcher: "*"},
				{Name: "Stop"},
			},
			ExtraToolLabels: map[string]string{
				"ApplyPatch": "Applying patch",
			},
		}),
	}
}

func (i *CodexHookInstaller) Install() error { return i.inner.Install() }
func (i *CodexHookInstaller) Uninstall()     { i.inner.Uninstall() }
