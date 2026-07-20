// package-plugin signs and packages a smuler plugin while preserving every
// manifest field (including authProviders, icon, oauth, lookupActions).
//
// The stock `smuler plugin sign` rewrite path drops unknown fields, which
// removed authProviders from published first-party packages.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	keygen := flag.Bool("keygen", false, "generate ~/.config/smuler/keys/smuler_ed25519 and print public key")
	pluginDir := flag.String("dir", ".", "plugin directory containing manifest.json")
	keyPath := flag.String("key", "", "ed25519 private key path (default: ~/.config/smuler/keys/smuler_ed25519)")
	outDir := flag.String("out", "", "output directory for archive (default: plugin dir)")
	flag.Parse()

	if *keyPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatal(err)
		}
		*keyPath = filepath.Join(home, ".config", "smuler", "keys", "smuler_ed25519")
	}

	if *keygen {
		if err := generateKey(*keyPath); err != nil {
			fatal(err)
		}
		return
	}

	if *outDir == "" {
		*outDir = *pluginDir
	}
	if err := packagePlugin(*pluginDir, *keyPath, *outDir); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "package-plugin: %v\n", err)
	os.Exit(1)
}

func generateKey(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	// Store seed (32 bytes) as base64 — matches common smuler keygen output shape.
	seed := priv.Seed()
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(seed)+"\n"), 0o600); err != nil {
		return err
	}
	pubPath := path + ".pub"
	if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote private key: %s\npublic key: %s\n", path, base64.StdEncoding.EncodeToString(pub))
	return nil
}

func packagePlugin(pluginDir, keyPath, outDir string) error {
	manifestPath := filepath.Join(pluginDir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	// Lightweight typed read for required fields / registry entry.
	var meta struct {
		ID                 string   `json:"id"`
		Name               string   `json:"name"`
		Version            string   `json:"version"`
		Description        string   `json:"description"`
		Executable         string   `json:"executable"`
		MinimumHostVersion string   `json:"minimumHostVersion"`
		Capabilities       []string `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if meta.ID == "" || meta.Version == "" || meta.Executable == "" {
		return fmt.Errorf("manifest missing id/version/executable")
	}

	priv, pub, err := loadKey(keyPath)
	if err != nil {
		return err
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	binPath := filepath.Join(pluginDir, meta.Executable)
	binData, err := os.ReadFile(binPath)
	if err != nil {
		return fmt.Errorf("read binary %s: %w", meta.Executable, err)
	}

	prettyWithPub, err := prepareManifestPretty(raw, pubB64)
	if err != nil {
		return err
	}
	// Sign the validate-compatible payload (decode + re-marshal), not raw
	// Compact bytes — otherwise non-ASCII strings diverge (\uXXXX vs UTF-8).
	unsignedCompact, err := manifestSigningPayload(prettyWithPub)
	if err != nil {
		return fmt.Errorf("signing payload: %w", err)
	}
	manifestSig := signSHA256(priv, unsignedCompact)
	binarySig := signSHA256(priv, binData)

	signedPretty, err := appendSignatures(prettyWithPub, manifestSig, binarySig)
	if err != nil {
		return err
	}

	stage, err := os.MkdirTemp("", "smuler-package-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	root := filepath.Join(stage, meta.ID)
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(meta.Executable)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), signedPretty, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, meta.Executable), binData, 0o755); err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	archiveName := fmt.Sprintf("%s-%s.tar.gz", meta.ID, meta.Version)
	archivePath := filepath.Join(outDir, archiveName)
	if err := writeTarGz(archivePath, root, meta.ID); err != nil {
		return err
	}

	sum, err := sha256File(archivePath)
	if err != nil {
		return err
	}

	entry := map[string]any{
		"id":                 meta.ID,
		"name":               meta.Name,
		"version":            meta.Version,
		"description":        meta.Description,
		"url":                fmt.Sprintf("https://github.com/YOUR_USER/YOUR_REPO/releases/download/v%s/%s", meta.Version, archiveName),
		"publicKey":          pubB64,
		"sha256":             sum,
		"minimumHostVersion": meta.MinimumHostVersion,
		"capabilities":       meta.Capabilities,
	}
	entryBytes, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	entryBytes = append(entryBytes, '\n')

	smulerDir := filepath.Join(pluginDir, ".smuler")
	if err := os.MkdirAll(smulerDir, 0o755); err != nil {
		return err
	}
	entryPath := filepath.Join(smulerDir, "registry-entry.json")
	if err := os.WriteFile(entryPath, entryBytes, 0o644); err != nil {
		return err
	}
	if outDir != pluginDir {
		if err := os.WriteFile(filepath.Join(outDir, fmt.Sprintf("%s-%s.json", meta.ID, meta.Version)), entryBytes, 0o644); err != nil {
			return err
		}
	}

	fmt.Printf("packed %s@%s\n  archive: %s\n  entry:   %s\n  sha256:  %s\n", meta.ID, meta.Version, archivePath, entryPath, sum)
	return nil
}

func loadKey(path string) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read key %s: %w (run: go run ./tools/package-plugin -keygen)", path, err)
	}
	raw := strings.TrimSpace(string(data))
	var priv ed25519.PrivateKey
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		switch len(decoded) {
		case ed25519.SeedSize:
			priv = ed25519.NewKeyFromSeed(decoded)
		case ed25519.PrivateKeySize:
			priv = ed25519.PrivateKey(decoded)
		default:
			return nil, nil, fmt.Errorf("unsupported key length %d in %s", len(decoded), path)
		}
	} else {
		b := bytes.TrimSpace(data)
		switch len(b) {
		case ed25519.SeedSize:
			priv = ed25519.NewKeyFromSeed(b)
		case ed25519.PrivateKeySize:
			priv = ed25519.PrivateKey(b)
		default:
			return nil, nil, fmt.Errorf("cannot parse key file %s", path)
		}
	}
	return priv, priv.Public().(ed25519.PublicKey), nil
}

func signSHA256(priv ed25519.PrivateKey, data []byte) string {
	sum := sha256.Sum256(data)
	sig := ed25519.Sign(priv, sum[:])
	return base64.StdEncoding.EncodeToString(sig)
}

// prepareManifestPretty returns pretty JSON with publicKey set and signatures removed.
func prepareManifestPretty(raw []byte, pubB64 string) ([]byte, error) {
	fields, err := parseObjectFields(raw)
	if err != nil {
		return nil, err
	}

	out := make([]objectField, 0, len(fields)+1)
	havePub := false
	for _, f := range fields {
		if f.key == "signatures" {
			continue
		}
		if f.key == "publicKey" {
			encoded, err := json.Marshal(pubB64)
			if err != nil {
				return nil, err
			}
			out = append(out, objectField{key: "publicKey", value: encoded})
			havePub = true
			continue
		}
		out = append(out, f)
	}
	if !havePub {
		encoded, err := json.Marshal(pubB64)
		if err != nil {
			return nil, err
		}
		out = append(out, objectField{key: "publicKey", value: encoded})
	}
	return encodeObjectPretty(out)
}

// manifestSigningPayload matches smuler-registry validate: compact JSON with
// signatures removed, field order preserved, values re-marshaled via encoding/json.
func manifestSigningPayload(manifestData []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(manifestData))
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("manifest root must be an object")
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("manifest object keys must be strings")
		}
		value, err := compactPreserveOrder(dec)
		if err != nil {
			return nil, err
		}
		if key == "signatures" {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(value)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func compactPreserveOrder(dec *json.Decoder) ([]byte, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			var buf bytes.Buffer
			buf.WriteByte('{')
			first := true
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("object keys must be strings")
				}
				value, err := compactPreserveOrder(dec)
				if err != nil {
					return nil, err
				}
				if !first {
					buf.WriteByte(',')
				}
				first = false
				keyJSON, err := json.Marshal(key)
				if err != nil {
					return nil, err
				}
				buf.Write(keyJSON)
				buf.WriteByte(':')
				buf.Write(value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			buf.WriteByte('}')
			return buf.Bytes(), nil
		case '[':
			var buf bytes.Buffer
			buf.WriteByte('[')
			first := true
			for dec.More() {
				value, err := compactPreserveOrder(dec)
				if err != nil {
					return nil, err
				}
				if !first {
					buf.WriteByte(',')
				}
				first = false
				buf.Write(value)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			buf.WriteByte(']')
			return buf.Bytes(), nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", v)
		}
	default:
		return json.Marshal(v)
	}
}

func appendSignatures(prettyUnsigned []byte, manifestSig, binarySig string) ([]byte, error) {
	fields, err := parseObjectFields(prettyUnsigned)
	if err != nil {
		return nil, err
	}
	sigObj := map[string]string{
		"manifest": manifestSig,
		"binary":   binarySig,
	}
	sigJSON, err := json.Marshal(sigObj)
	if err != nil {
		return nil, err
	}
	fields = append(fields, objectField{key: "signatures", value: sigJSON})
	out, err := encodeObjectPretty(fields)
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type objectField struct {
	key   string
	value json.RawMessage
}

func parseObjectFields(raw []byte) ([]objectField, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("manifest root must be an object")
	}
	var fields []objectField
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("object keys must be strings")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, objectField{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return fields, nil
}

func encodeObjectPretty(fields []objectField) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, f := range fields {
		keyJSON, err := json.Marshal(f.key)
		if err != nil {
			return nil, err
		}
		// Re-marshal through encoding/json so on-disk bytes match the
		// validate-compatible signing payload for strings/numbers.
		var parsed any
		if err := json.Unmarshal(f.value, &parsed); err != nil {
			return nil, err
		}
		prettyVal, err := json.MarshalIndent(parsed, "  ", "  ")
		if err != nil {
			return nil, err
		}
		buf.WriteString("  ")
		buf.Write(keyJSON)
		buf.WriteString(": ")
		buf.Write(prettyVal)
		if i < len(fields)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeTarGz(archivePath, dir, rootName string) error {
	f, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join(rootName, rel))
		if info.IsDir() {
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name + "/"
			return tw.WriteHeader(hdr)
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
