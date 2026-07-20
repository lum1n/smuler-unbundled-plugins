package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPackagePreservesAuthProvidersAndVerifies(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "demo")
	if err := os.MkdirAll(filepath.Join(pluginDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := map[string]any{
		"id":          "demo",
		"name":        "Demo",
		"version":     "0.1.1",
		"description": "demo with non-ascii — dash",
		"executable":  "bin/demo-plugin",
		"capabilities": []string{
			"summary",
		},
		"minimumHostVersion": "0.1.0",
		"authProviders": []map[string]any{
			{
				"id":          "demo-browser",
				"displayName": "Cookie Auth",
				"authKind":    "browser_import",
			},
		},
		"icon": map[string]any{"type": "sfSymbol", "value": "star"},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := []byte("fake-binary-contents")
	if err := os.WriteFile(filepath.Join(pluginDir, "bin", "demo-plugin"), bin, 0o755); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(dir, "key")
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(dir, "out")
	if err := packagePlugin(pluginDir, keyPath, outDir); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(outDir, "demo-0.1.1.tar.gz")
	manifestBytes, binaryBytes := mustReadArchive(t, archive, "demo/manifest.json", "demo/bin/demo-plugin")

	var packed map[string]any
	if err := json.Unmarshal(manifestBytes, &packed); err != nil {
		t.Fatal(err)
	}
	aps, ok := packed["authProviders"].([]any)
	if !ok || len(aps) != 1 {
		t.Fatalf("authProviders missing: %#v", packed["authProviders"])
	}
	if _, ok := packed["icon"]; !ok {
		t.Fatal("icon stripped from packaged manifest")
	}

	payload, err := manifestSigningPayload(manifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	sigs := packed["signatures"].(map[string]any)
	if !ed25519.Verify(pub, sha256sum(payload), mustDecodeB64(t, sigs["manifest"].(string))) {
		t.Fatal("manifest signature mismatch")
	}
	if !ed25519.Verify(pub, sha256sum(binaryBytes), mustDecodeB64(t, sigs["binary"].(string))) {
		t.Fatal("binary signature mismatch")
	}
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func mustDecodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustReadArchive(t *testing.T, archive string, names ...string) ([]byte, []byte) {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	found := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if hdr.Name == name {
				b, err := io.ReadAll(tr)
				if err != nil {
					t.Fatal(err)
				}
				found[name] = b
			}
		}
	}
	out := make([][]byte, len(names))
	for i, name := range names {
		b, ok := found[name]
		if !ok {
			t.Fatalf("missing %s in archive", name)
		}
		out[i] = b
	}
	return out[0], out[1]
}
