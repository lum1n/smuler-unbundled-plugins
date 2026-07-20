package rules

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Scanner discovers agent configuration files inside repo roots.
type Scanner struct {
	maxExcerptLen int
}

// NewScanner creates a scanner with sensible defaults.
func NewScanner() *Scanner {
	return &Scanner{maxExcerptLen: 180}
}

// Scan walks the given repo roots and returns discovered rule files.
// Only repos with running agents are scanned, per product decision.
func (s *Scanner) Scan(repoRoots []string) []RuleFile {
	seen := make(map[string]bool)
	var out []RuleFile

	for _, root := range repoRoots {
		root = filepath.Clean(root)
		if root == "" || root == "." {
			continue
		}
		if seen[root] {
			continue
		}
		seen[root] = true

		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}

		repoName := filepath.Base(root)
		for _, target := range s.targets() {
			matches, err := filepath.Glob(filepath.Join(root, target.glob))
			if err != nil {
				continue
			}
			for _, p := range matches {
				if info, err := os.Stat(p); err == nil && !info.IsDir() {
					out = append(out, s.readFile(p, root, repoName, target.kind))
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].RepoName != out[j].RepoName {
			return out[i].RepoName < out[j].RepoName
		}
		return out[i].Path < out[j].Path
	})
	return out
}

type scanTarget struct {
	glob string
	kind RuleKind
}

func (s *Scanner) targets() []scanTarget {
	return []scanTarget{
		{glob: "AGENTS.md", kind: KindAgentsMd},
		{glob: ".cursor/rules/*", kind: KindCursorRule},
		{glob: ".claude/CLAUDE.md", kind: KindClaudeMd},
		{glob: ".claude/skills/*/SKILL.md", kind: KindClaudeSkill},
		{glob: ".codex/settings.json", kind: KindCodexConfig},
		{glob: ".pi/SYSTEM.md", kind: KindPiSystem},
		{glob: ".opencode/opencode.json", kind: KindOpenCode},
		{glob: ".aider*", kind: KindAiderConfig},
	}
}

func (s *Scanner) readFile(path, root, repoName string, kind RuleKind) RuleFile {
	info, err := os.Stat(path)
	if err != nil {
		info = &fsFileInfo{}
	}

	content, _ := os.ReadFile(path)
	title := extractTitle(string(content), filepath.Base(path))
	excerpt := extractExcerpt(string(content), s.maxExcerptLen)

	return RuleFile{
		Path:         path,
		RepoRoot:     root,
		RepoName:     repoName,
		Kind:         kind,
		Title:        title,
		Excerpt:      excerpt,
		LastModified: info.ModTime(),
		Size:         info.Size(),
	}
}

func extractTitle(content, fallback string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Markdown heading.
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		// JSON file: use filename without extension.
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			return fallback
		}
		// First non-empty line for plain text.
		return line
	}
	return fallback
}

func extractExcerpt(content string, max int) string {
	lines := strings.Split(content, "\n")
	var parts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		parts = append(parts, line)
		if len(strings.Join(parts, " ")) > max {
			break
		}
	}
	s := strings.Join(parts, " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

type fsFileInfo struct{}

func (fsFileInfo) Name() string       { return "" }
func (fsFileInfo) Size() int64        { return 0 }
func (fsFileInfo) Mode() fs.FileMode  { return 0 }
func (fsFileInfo) ModTime() time.Time { return time.Time{} }
func (fsFileInfo) IsDir() bool        { return false }
func (fsFileInfo) Sys() any           { return nil }
