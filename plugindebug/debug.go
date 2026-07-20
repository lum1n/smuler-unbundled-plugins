package plugindebug

import (
	"fmt"
	"os"
	"strings"
)

const (
	ConfigKey = "pluginDebugLogging"
	EnvVar    = "SMULER_PLUGIN_DEBUG"
)

var hostEnabled bool

func ConfigureFromInitializeConfig(config map[string]string) {
	if config == nil {
		hostEnabled = false
		return
	}
	hostEnabled = parseTruthy(config[ConfigKey])
}

func parseTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func Enabled() bool {
	if hostEnabled {
		return true
	}
	return parseTruthy(os.Getenv(EnvVar))
}

func Log(prefix, format string, args ...interface{}) {
	if !Enabled() {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, strings.TrimSpace(prefix)+" "+format+"\n", args...)
}

func Reset() {
	hostEnabled = false
}
