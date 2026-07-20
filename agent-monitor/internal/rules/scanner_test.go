package rules

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScannerDiscoversKnownFiles(t *testing.T) {
	tmp := t.TempDir()

	makeFile(t, filepath.Join(tmp, "AGENTS.md"), "# Project Rules\n\nUse 2-space indent.")
	makeFile(t, filepath.Join(tmp, ".cursor", "rules", "style.mdc"), "# Style\nAlways use semicolons.")
	makeFile(t, filepath.Join(tmp, ".claude", "CLAUDE.md"), "# Claude Project\n\nBe concise.")
	makeFile(t, filepath.Join(tmp, ".claude", "skills", "debug", "SKILL.md"), "# Debug Skill\n\nCheck logs first.")
	makeFile(t, filepath.Join(tmp, ".codex", "settings.json"), "{\"hooks\": []}")
	makeFile(t, filepath.Join(tmp, ".pi", "SYSTEM.md"), "# System\n\nYou are a helpful assistant.")

	scanner := NewScanner()
	files := scanner.Scan([]string{tmp})

	if len(files) != 6 {
		t.Fatalf("expected 6 rule files, got %d", len(files))
	}

	kinds := make(map[RuleKind]int)
	for _, f := range files {
		kinds[f.Kind]++
	}

	for _, kind := range []RuleKind{KindAgentsMd, KindCursorRule, KindClaudeMd, KindClaudeSkill, KindCodexConfig, KindPiSystem} {
		if kinds[kind] != 1 {
			t.Fatalf("expected exactly one %s, got %d", kind, kinds[kind])
		}
	}

	agents := findByKind(files, KindAgentsMd)
	if agents == nil {
		t.Fatal("expected to find agents_md file")
	}
	if agents.Title != "Project Rules" {
		t.Fatalf("expected title 'Project Rules', got %q", agents.Title)
	}
	if agents.Excerpt == "" {
		t.Fatal("expected non-empty excerpt")
	}
}

func TestScannerSkipsNonexistentRepos(t *testing.T) {
	scanner := NewScanner()
	files := scanner.Scan([]string{"/definitely/not/a/repo"})
	if len(files) != 0 {
		t.Fatalf("expected 0 files, got %d", len(files))
	}
}

func TestScannerDedupesRepoRoots(t *testing.T) {
	tmp := t.TempDir()
	makeFile(t, filepath.Join(tmp, "AGENTS.md"), "# Rules")

	scanner := NewScanner()
	files := scanner.Scan([]string{tmp, tmp})
	if len(files) != 1 {
		t.Fatalf("expected 1 file after dedupe, got %d", len(files))
	}
}

func findByKind(files []RuleFile, kind RuleKind) *RuleFile {
	for i := range files {
		if files[i].Kind == kind {
			return &files[i]
		}
	}
	return nil
}

func makeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
}
