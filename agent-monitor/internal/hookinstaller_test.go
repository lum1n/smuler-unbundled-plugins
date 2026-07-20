package internal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testConfig(settingsPath string) NativeHookConfig {
	return NativeHookConfig{
		AgentID:      "test-agent",
		AgentName:    "Test Agent",
		ScriptName:   "test-hook.js",
		SettingsPath: settingsPath,
		BackupSuffix: ".test.bak",
		Events: []HookEventConfig{
			{Name: "prompt"},
			{Name: "pre", Matcher: "*"},
			{Name: "stop"},
		},
		ExtraToolLabels: map[string]string{"CustomTool": "Custom"},
	}
}

func TestNativeHookInstallerInstallsHooksWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, "settings.json")
	installer := NewNativeHookInstaller(testConfig(settingsPath))
	installer.scriptPath = filepath.Join(tmp, "test-hook.js")

	nodePath := "/fake/node"
	settings := installer.loadSettings(settingsPath)
	if installer.isInstalled(settings, nodePath) {
		t.Fatal("expected hooks to not be installed initially")
	}

	installer.apply(settings, nodePath)
	if !installer.isInstalled(settings, nodePath) {
		t.Fatal("expected hooks to be installed after apply")
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	// Running again on same file should report installed.
	settings2 := installer.loadSettings(settingsPath)
	if !installer.isInstalled(settings2, nodePath) {
		t.Fatal("expected hooks to still be installed after reload")
	}
}

func TestNativeHookInstallerPreservesUnrelatedHooks(t *testing.T) {
	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, "settings.json")
	installer := NewNativeHookInstaller(testConfig(settingsPath))
	installer.scriptPath = filepath.Join(tmp, "test-hook.js")

	initial := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"prompt": []any{
				map[string]any{
					"matcher": "*",
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/bin/true"},
					},
				},
			},
		},
	}
	writeJSON(t, settingsPath, initial)

	settings := installer.loadSettings(settingsPath)
	installer.apply(settings, "/fake/node")

	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatal("expected hooks section")
	}
	promptHooks, ok := hooks["prompt"].([]any)
	if !ok || len(promptHooks) != 2 {
		t.Fatalf("expected 2 prompt hook entries, got %d", len(promptHooks))
	}

	first, ok := promptHooks[0].(map[string]any)
	if !ok {
		t.Fatal("expected first hook entry to be preserved")
	}
	firstCmds, _ := first["hooks"].([]any)
	if len(firstCmds) != 1 {
		t.Fatalf("expected unrelated hook to be preserved, got %d", len(firstCmds))
	}
	cmd, _ := firstCmds[0].(map[string]any)["command"].(string)
	if cmd != "/usr/bin/true" {
		t.Fatalf("expected preserved command /usr/bin/true, got %s", cmd)
	}
}

func TestNativeHookInstallerBackupOnlyOnChange(t *testing.T) {
	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, "settings.json")
	bakPath := settingsPath + ".test.bak"
	installer := NewNativeHookInstaller(testConfig(settingsPath))
	installer.scriptPath = filepath.Join(tmp, "test-hook.js")

	writeJSON(t, settingsPath, map[string]any{"theme": "light"})

	if err := installer.backup(settingsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bakPath); err != nil {
		t.Fatal("expected backup to be created")
	}

	// Second backup should be skipped.
	if err := os.WriteFile(settingsPath, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installer.backup(settingsPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "modified" {
		t.Fatal("expected backup to keep original contents")
	}
}

func TestNativeHookInstallerUninstallRemovesOnlyOurs(t *testing.T) {
	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, "settings.json")
	installer := NewNativeHookInstaller(testConfig(settingsPath))
	installer.scriptPath = filepath.Join(tmp, "test-hook.js")

	initial := map[string]any{
		"hooks": map[string]any{
			"prompt": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/bin/true"},
						map[string]any{"type": "command", "command": "/fake/node " + installer.scriptPath + " prompt"},
					},
				},
			},
			"stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": "/fake/node " + installer.scriptPath + " stop"},
					},
				},
			},
		},
	}
	writeJSON(t, settingsPath, initial)

	installer.Uninstall()

	settings := installer.loadSettings(settingsPath)
	hooks, _ := settings["hooks"].(map[string]any)
	promptHooks, _ := hooks["prompt"].([]any)
	if len(promptHooks) != 1 {
		t.Fatalf("expected 1 prompt hook entry after uninstall, got %d", len(promptHooks))
	}
	entry, _ := promptHooks[0].(map[string]any)
	cmds, _ := entry["hooks"].([]any)
	if len(cmds) != 1 {
		t.Fatalf("expected unrelated command to remain, got %d", len(cmds))
	}

	if _, ok := hooks["stop"]; ok {
		t.Fatal("expected stop hook section to be removed entirely")
	}
}

func TestNativeHookInstallerHookScriptContainsLabels(t *testing.T) {
	tmp := t.TempDir()
	settingsPath := filepath.Join(tmp, "settings.json")
	cfg := testConfig(settingsPath)
	installer := NewNativeHookInstaller(cfg)
	installer.scriptPath = filepath.Join(tmp, "test-hook.js")

	script := installer.hookScript()
	if script == "" {
		t.Fatal("expected non-empty script")
	}
	for _, label := range []string{"Running command", "Editing", "Custom"} {
		if !contains(script, label) {
			t.Fatalf("expected script to contain label %q", label)
		}
	}
	if !contains(script, `agentId:"test-agent"`) {
		t.Fatal("expected script to contain agentId")
	}
	if !contains(script, `UserPromptSubmit: "prompt"`) {
		t.Fatal("expected script to map Claude/Codex event names")
	}
	if !contains(script, `case "post": state = "thinking"`) {
		t.Fatal("expected post-tool hook to set thinking state")
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
