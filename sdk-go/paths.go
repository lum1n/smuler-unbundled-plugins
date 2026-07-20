package sdk

import (
	"os"
	"path/filepath"
)

// HostPaths are real filesystem locations injected by the macOS host during initialize.
// Prefer these keys over process HOME when resolving user config and cache directories.
type HostPaths struct {
	RealHome  string
	ConfigDir string
	CacheDir  string
}

// HostPathsFromConfig reads host.realHome, host.configDir, and host.cacheDir from initialize config.
func HostPathsFromConfig(config map[string]string) HostPaths {
	paths := HostPaths{}
	if v := config["host.realHome"]; v != "" {
		paths.RealHome = v
	} else if v := os.Getenv("SMULER_REAL_HOME"); v != "" {
		paths.RealHome = v
	} else if v := os.Getenv("HOME"); v != "" {
		paths.RealHome = v
	} else {
		paths.RealHome, _ = os.UserHomeDir()
	}

	if v := config["host.configDir"]; v != "" {
		paths.ConfigDir = v
	} else if v := os.Getenv("SMULER_CONFIG_DIR"); v != "" {
		paths.ConfigDir = v
	} else {
		paths.ConfigDir = filepath.Join(paths.RealHome, ".config", "smuler")
	}

	if v := config["host.cacheDir"]; v != "" {
		paths.CacheDir = v
	} else if v := os.Getenv("SMULER_CACHE_DIR"); v != "" {
		paths.CacheDir = v
	} else {
		paths.CacheDir = filepath.Join(paths.RealHome, ".cache", "smuler")
	}

	return paths
}
