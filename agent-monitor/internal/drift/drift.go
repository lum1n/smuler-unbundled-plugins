package drift

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind classifies a drift suggestion.
type Kind string

const (
	KindContradiction   Kind = "contradiction"
	KindMissingRule     Kind = "missing_rule"
	KindConflictingAgents Kind = "conflicting_agents"
	KindStaleRule       Kind = "stale_rule"
)

// Severity indicates how strongly to surface the suggestion.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Suggestion is a single drift-detection proposal.
type Suggestion struct {
	ID            string   `json:"id"`
	Kind          Kind     `json:"kind"`
	Severity      Severity `json:"severity"`
	Repo          string   `json:"repo"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Evidence      []string `json:"evidence"`
	ProposedPath  string   `json:"proposedPath"`
	ProposedPatch string   `json:"proposedPatch"`
	Status        string   `json:"status"`
	SnoozedUntil  int64    `json:"snoozedUntil"`
	CreatedAt     int64    `json:"createdAt"`
}

// IsActive returns true if the suggestion is neither dismissed nor snoozed.
func (s Suggestion) IsActive() bool {
	if s.Status == "dismissed" {
		return false
	}
	if s.Status == "snoozed" && s.SnoozedUntil > time.Now().UnixMilli() {
		return false
	}
	return true
}

// contradictionPair defines two opposing concepts for simple matching.
type contradictionPair struct {
	a, b string
}

var defaultContradictions = []contradictionPair{
	{"tabs", "spaces"},
	{"semicolons", "no semicolons"},
	{"var ", "const "},
	{"var ", "let "},
	{"double quotes", "single quotes"},
	{" trailing ", "no trailing"},
}

var phrasePattern = regexp.MustCompile(`"([^"]{5,80})"|'([^']{5,80})'|\b(use|always|never|avoid|prefer)\b[^.]{3,60}`)

// Analyzer detects drift between agent behavior and project rules.
type Analyzer struct {
	storePath     string
	contradictions []contradictionPair
}

// NewAnalyzer creates an analyzer with the default persistence path.
func NewAnalyzer() *Analyzer {
	home, _ := os.UserHomeDir()
	return &Analyzer{
		storePath:      filepath.Join(home, ".cache", "smuler", "agent-monitor-suggestions.json"),
		contradictions: defaultContradictions,
	}
}

// Analyze compares rules and agent sessions and returns active suggestions.
func (a *Analyzer) Analyze(repo string, rules []string, sessions []Session) []Suggestion {
	existing := a.load()
	byID := make(map[string]Suggestion)
	for _, s := range existing {
		byID[s.ID] = s
	}

	newSuggestions := []Suggestion{}
	newSuggestions = append(newSuggestions, a.detectContradictions(repo, rules, sessions)...)
	newSuggestions = append(newSuggestions, a.detectMissingRules(repo, rules, sessions)...)
	newSuggestions = append(newSuggestions, a.detectConflictingAgents(repo, sessions)...)

	now := time.Now().UnixMilli()
	for _, ns := range newSuggestions {
		if old, ok := byID[ns.ID]; ok {
			// Preserve status/snooze from existing suggestion.
			ns.Status = old.Status
			ns.SnoozedUntil = old.SnoozedUntil
			ns.CreatedAt = old.CreatedAt
		} else {
			ns.CreatedAt = now
		}
		byID[ns.ID] = ns
	}

	out := make([]Suggestion, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})

	a.save(out)
	return a.active(out)
}

// Load returns all stored suggestions (including dismissed/snoozed).
func (a *Analyzer) Load() []Suggestion {
	return a.load()
}

// Dismiss marks a suggestion dismissed.
func (a *Analyzer) Dismiss(id string) error {
	suggestions := a.load()
	for i := range suggestions {
		if suggestions[i].ID == id {
			suggestions[i].Status = "dismissed"
			return a.save(suggestions)
		}
	}
	return nil
}

// Snooze marks a suggestion snoozed until the given timestamp.
func (a *Analyzer) Snooze(id string, until int64) error {
	suggestions := a.load()
	for i := range suggestions {
		if suggestions[i].ID == id {
			suggestions[i].Status = "snoozed"
			suggestions[i].SnoozedUntil = until
			return a.save(suggestions)
		}
	}
	return nil
}

// ApplySuggestion writes the proposed patch to the proposed path.
// It creates the file if it does not exist, or appends if it does.
func (a *Analyzer) ApplySuggestion(s Suggestion) error {
	if s.ProposedPath == "" {
		return fmt.Errorf("no proposed path")
	}
	if err := os.MkdirAll(filepath.Dir(s.ProposedPath), 0750); err != nil {
		return err
	}

	var content []byte
	if info, err := os.Stat(s.ProposedPath); err == nil && !info.IsDir() {
		content, _ = os.ReadFile(s.ProposedPath)
		if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
			content = append(content, '\n')
		}
	}
	content = append(content, []byte(s.ProposedPatch)...)
	if !strings.HasSuffix(s.ProposedPatch, "\n") {
		content = append(content, '\n')
	}
	if err := os.WriteFile(s.ProposedPath, content, 0640); err != nil {
		return err
	}
	return a.Dismiss(s.ID)
}

func (a *Analyzer) active(suggestions []Suggestion) []Suggestion {
	var out []Suggestion
	for _, s := range suggestions {
		if s.IsActive() {
			out = append(out, s)
		}
	}
	return out
}

func (a *Analyzer) load() []Suggestion {
	data, err := os.ReadFile(a.storePath)
	if err != nil {
		return nil
	}
	var out []Suggestion
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return out
}

func (a *Analyzer) save(suggestions []Suggestion) error {
	if err := os.MkdirAll(filepath.Dir(a.storePath), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(suggestions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.storePath, append(data, '\n'), 0600)
}

func (a *Analyzer) detectContradictions(repo string, rules []string, sessions []Session) []Suggestion {
	var out []Suggestion
	rulesText := strings.ToLower(strings.Join(rules, "\n"))

	for _, sess := range sessions {
		task := strings.ToLower(sess.Task)
		if task == "" {
			continue
		}
		for _, pair := range a.contradictions {
			aInRule := strings.Contains(rulesText, pair.a)
			bInRule := strings.Contains(rulesText, pair.b)
			taskHasA := strings.Contains(task, pair.a)
			taskHasB := strings.Contains(task, pair.b)

			if aInRule && taskHasB && !taskHasA {
				out = append(out, Suggestion{
					ID:           fmt.Sprintf("contradiction-%s-%s-%s", repo, pair.a, sess.ID),
					Kind:         KindContradiction,
					Severity:     SeverityWarning,
					Repo:         repo,
					Title:        fmt.Sprintf("Possible contradiction with %s rule", pair.a),
					Description:  fmt.Sprintf("Rules mention '%s', but a recent agent task asks for '%s'.", pair.a, pair.b),
					Evidence:     []string{sess.Task},
					ProposedPath: filepath.Join("AGENTS.md"),
					ProposedPatch: fmt.Sprintf("\n## %s\nWhen refactoring, prefer %s to stay consistent with existing rules.\n", pair.a, pair.a),
				})
			}
			if bInRule && taskHasA && !taskHasB {
				out = append(out, Suggestion{
					ID:           fmt.Sprintf("contradiction-%s-%s-%s", repo, pair.b, sess.ID),
					Kind:         KindContradiction,
					Severity:     SeverityWarning,
					Repo:         repo,
					Title:        fmt.Sprintf("Possible contradiction with %s rule", pair.b),
					Description:  fmt.Sprintf("Rules mention '%s', but a recent agent task asks for '%s'.", pair.b, pair.a),
					Evidence:     []string{sess.Task},
					ProposedPath: filepath.Join("AGENTS.md"),
					ProposedPatch: fmt.Sprintf("\n## %s\nWhen refactoring, prefer %s to stay consistent with existing rules.\n", pair.b, pair.b),
				})
			}
		}
	}
	return out
}

func (a *Analyzer) detectMissingRules(repo string, rules []string, sessions []Session) []Suggestion {
	if len(sessions) < 2 {
		return nil
	}

	rulesText := strings.ToLower(strings.Join(rules, "\n"))
	phraseCounts := make(map[string]int)
	phraseExamples := make(map[string]string)

	for _, sess := range sessions {
		for _, match := range phrasePattern.FindAllStringSubmatch(sess.Task, -1) {
			phrase := strings.TrimSpace(match[0])
			if phrase == "" || len(phrase) < 10 {
				continue
			}
			phrase = strings.ToLower(phrase)
			if strings.Contains(rulesText, phrase) {
				continue
			}
			phraseCounts[phrase]++
			if phraseExamples[phrase] == "" {
				phraseExamples[phrase] = sess.Task
			}
		}
	}

	var out []Suggestion
	for phrase, count := range phraseCounts {
		if count >= 2 {
			out = append(out, Suggestion{
				ID:           fmt.Sprintf("missing-rule-%s-%s", repo, hashPhrase(phrase)),
				Kind:         KindMissingRule,
				Severity:     SeverityInfo,
				Repo:         repo,
				Title:        "Consider codifying repeated instruction",
				Description:  fmt.Sprintf("The instruction '%s' appeared in %d agent sessions but is not in any rule file.", phrase, count),
				Evidence:     []string{phraseExamples[phrase]},
				ProposedPath: filepath.Join("AGENTS.md"),
				ProposedPatch: fmt.Sprintf("\n## Repeated instruction\n%s\n", phrase),
			})
		}
	}
	return out
}

func (a *Analyzer) detectConflictingAgents(repo string, sessions []Session) []Suggestion {
	if len(sessions) < 2 {
		return nil
	}

	var out []Suggestion
	for i := 0; i < len(sessions); i++ {
		for j := i + 1; j < len(sessions); j++ {
			sa, sb := sessions[i], sessions[j]
			if sa.Task == "" || sb.Task == "" {
				continue
			}
			ta, tb := strings.ToLower(sa.Task), strings.ToLower(sb.Task)
			for _, pair := range a.contradictions {
				aInA := strings.Contains(ta, pair.a)
				bInB := strings.Contains(tb, pair.b)
				bInA := strings.Contains(ta, pair.b)
				aInB := strings.Contains(tb, pair.a)
				if (aInA && bInB) || (bInA && aInB) {
					out = append(out, Suggestion{
						ID:           fmt.Sprintf("conflict-%s-%s-%s", repo, sa.ID, sb.ID),
						Kind:         KindConflictingAgents,
						Severity:     SeverityCritical,
						Repo:         repo,
						Title:        "Conflicting agent instructions",
						Description:  fmt.Sprintf("Two agents on %s received opposing instructions about '%s' vs '%s'.", repo, pair.a, pair.b),
						Evidence:     []string{sa.Task, sb.Task},
						ProposedPath: filepath.Join("AGENTS.md"),
						ProposedPatch: fmt.Sprintf("\n## Conflict resolution\nClarify whether to use %s or %s across the project.\n", pair.a, pair.b),
					})
				}
			}
		}
	}
	return out
}

func hashPhrase(s string) string {
	h := 0
	for _, r := range s {
		h = h*31 + int(r)
		if h < 0 {
			h = -h
		}
	}
	return fmt.Sprintf("%x", h)[:8]
}

// Session is a lightweight input for drift analysis.
type Session struct {
	ID   string
	Task string
}
