package drift

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectContradiction(t *testing.T) {
	a := NewAnalyzer()
	a.storePath = filepath.Join(t.TempDir(), "suggestions.json")

	rules := []string{"Use spaces for indentation."}
	sessions := []Session{{ID: "s1", Task: "Convert everything to tabs."}}

	suggestions := a.Analyze("myrepo", rules, sessions)
	if len(suggestions) == 0 {
		t.Fatal("expected contradiction suggestion")
	}
	if suggestions[0].Kind != KindContradiction {
		t.Fatalf("expected contradiction, got %s", suggestions[0].Kind)
	}
}

func TestDetectMissingRule(t *testing.T) {
	a := NewAnalyzer()
	a.storePath = filepath.Join(t.TempDir(), "suggestions.json")

	rules := []string{"Be concise."}
	sessions := []Session{
		{ID: "s1", Task: `Always "preserve the public API" when refactoring.`},
		{ID: "s2", Task: `Make sure you "preserve the public API" in this refactor.`},
	}

	suggestions := a.Analyze("myrepo", rules, sessions)
	found := false
	for _, s := range suggestions {
		if s.Kind == KindMissingRule && s.Evidence[0] != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected missing rule suggestion")
	}
}

func TestDetectConflictingAgents(t *testing.T) {
	a := NewAnalyzer()
	a.storePath = filepath.Join(t.TempDir(), "suggestions.json")

	sessions := []Session{
		{ID: "s1", Task: "Convert indentation to tabs."},
		{ID: "s2", Task: "Use spaces for indentation."},
	}

	suggestions := a.Analyze("myrepo", nil, sessions)
	found := false
	for _, s := range suggestions {
		if s.Kind == KindConflictingAgents {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected conflicting agents suggestion")
	}
}

func TestDismissAndSnooze(t *testing.T) {
	a := NewAnalyzer()
	a.storePath = filepath.Join(t.TempDir(), "suggestions.json")

	suggestions := a.Analyze("myrepo", []string{"Use spaces."}, []Session{{ID: "s1", Task: "Use tabs."}})
	id := suggestions[0].ID

	if err := a.Dismiss(id); err != nil {
		t.Fatal(err)
	}
	active := a.Analyze("myrepo", []string{"Use spaces."}, []Session{{ID: "s1", Task: "Use tabs."}})
	for _, s := range active {
		if s.ID == id {
			t.Fatal("dismissed suggestion should not be active")
		}
	}
}

func TestApplySuggestion(t *testing.T) {
	a := NewAnalyzer()
	a.storePath = filepath.Join(t.TempDir(), "suggestions.json")

	tmp := t.TempDir()
	s := Suggestion{
		ID:            "apply-test",
		Kind:          KindMissingRule,
		Severity:      SeverityInfo,
		Repo:          "myrepo",
		Title:         "Test",
		ProposedPath:  filepath.Join(tmp, "AGENTS.md"),
		ProposedPatch: "\n## Test rule\nHello.\n",
	}
	if err := a.ApplySuggestion(s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.ProposedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "Test rule") {
		t.Fatalf("expected applied patch in file, got %s", string(data))
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
