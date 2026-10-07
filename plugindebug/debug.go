package plugindebug

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

const (
	ConfigKey = "pluginDebugLogging"
	EnvVar    = "SMULER_PLUGIN_DEBUG"
)

// hostEnabled is read from logging goroutines while initialize may update it.
var hostEnabled atomic.Bool

func ConfigureFromInitializeConfig(config map[string]string) {
	if config == nil {
		hostEnabled.Store(false)
		return
	}
	hostEnabled.Store(parseTruthy(config[ConfigKey]))
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
	if hostEnabled.Load() {
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
	hostEnabled.Store(false)
}
