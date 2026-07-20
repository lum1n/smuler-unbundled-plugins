package sdk

import (
	"path/filepath"
	"testing"
)

func TestHostPathsFromConfigPrefersHostKeys(t *testing.T) {
	paths := HostPathsFromConfig(map[string]string{
		"host.realHome":  "/Users/alice",
		"host.configDir": "/Users/alice/.config/smuler",
		"host.cacheDir":  "/Users/alice/.cache/smuler",
	})
	if paths.RealHome != "/Users/alice" {
		t.Fatalf("RealHome = %q", paths.RealHome)
	}
	if paths.ConfigDir != "/Users/alice/.config/smuler" {
		t.Fatalf("ConfigDir = %q", paths.ConfigDir)
	}
	if paths.CacheDir != "/Users/alice/.cache/smuler" {
		t.Fatalf("CacheDir = %q", paths.CacheDir)
	}
}

func TestHostPathsFromConfigDefaultsUnderHome(t *testing.T) {
	t.Setenv("HOME", "/Users/bob")
	t.Setenv("SMULER_CONFIG_DIR", "")
	t.Setenv("SMULER_CACHE_DIR", "")
	paths := HostPathsFromConfig(nil)
	if paths.ConfigDir != filepath.Join("/Users/bob", ".config", "smuler") {
		t.Fatalf("ConfigDir = %q", paths.ConfigDir)
	}
	if paths.CacheDir != filepath.Join("/Users/bob", ".cache", "smuler") {
		t.Fatalf("CacheDir = %q", paths.CacheDir)
	}
}
