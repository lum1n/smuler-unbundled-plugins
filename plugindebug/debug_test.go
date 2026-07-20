package plugindebug

import (
	"testing"
)

func TestDebugDisabledByDefault(t *testing.T) {
	Reset()
	t.Setenv(EnvVar, "")

	if Enabled() {
		t.Fatal("expected debug logging disabled by default")
	}
}

func TestDebugEnabledFromEnv(t *testing.T) {
	Reset()
	t.Setenv(EnvVar, "1")

	if !Enabled() {
		t.Fatal("expected SMULER_PLUGIN_DEBUG=1 to enable debug logging")
	}
}

func TestDebugEnabledFromInitializeConfig(t *testing.T) {
	Reset()
	t.Setenv(EnvVar, "")

	ConfigureFromInitializeConfig(map[string]string{ConfigKey: "true"})
	if !Enabled() {
		t.Fatal("expected pluginDebugLogging=true in initialize config to enable debug")
	}
}

func TestDebugLogIsNoOpWhenDisabled(t *testing.T) {
	Reset()
	t.Setenv(EnvVar, "")

	Log("[test]", "should not appear")
}
