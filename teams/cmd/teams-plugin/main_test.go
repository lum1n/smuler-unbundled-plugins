package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTailLogReturnsLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.txt")
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "line %d some padding text to make it longer\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 100 {
		t.Fatalf("got %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[0], "line 49900 ") || !strings.HasPrefix(lines[99], "line 49999 ") {
		t.Fatalf("unexpected bounds %q .. %q", lines[0], lines[99])
	}
}

func TestTailLogSmallFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.txt")
	if err := os.WriteFile(path, []byte("a\r\nb\nc"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLog(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "a,b,c" {
		t.Fatalf("got %q", lines)
	}
	empty := filepath.Join(t.TempDir(), "empty.txt")
	_ = os.WriteFile(empty, nil, 0o600)
	if lines, err := tailLog(empty, 10); err != nil || len(lines) != 0 {
		t.Fatalf("empty: %v %v", lines, err)
	}
}

func TestExtractLogStreamTimestampLocal(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	for _, layout := range []string{"2006-01-02 15:04:05.000", "2006-01-02 15:04:05.000000-0700"} {
		line := now.Format(layout) + " Df MSTeams[123] something"
		got := extractLogStreamTimestamp(line)
		if got.IsZero() || got.Sub(now).Abs() > time.Second {
			t.Fatalf("layout %q: got %v want ~%v", layout, got, now)
		}
	}
}

func TestParseEventsStableIDsAndISOTimestamps(t *testing.T) {
	lines := []string{
		`2026-06-30 14:20:45 notification "text": "hello there"`,
		`2026-06-30 14:20:45 @mention: "text": "ping"`,
		`no timestamp incomingCall`,
	}
	a := buildItems(parseEvents(lines), &handler{})
	b := buildItems(parseEvents(lines), &handler{})
	if len(a) != 3 {
		t.Fatalf("got %d items: %+v", len(a), a)
	}
	seen := map[string]bool{}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("unstable id %q vs %q", a[i].ID, b[i].ID)
		}
		if seen[a[i].ID] {
			t.Fatalf("duplicate id %q", a[i].ID)
		}
		seen[a[i].ID] = true
		if a[i].Timestamp != "" {
			if _, err := time.Parse(time.RFC3339, a[i].Timestamp); err != nil {
				t.Fatalf("timestamp %q not RFC3339", a[i].Timestamp)
			}
		}
	}
}

func TestJoinRejectsNonMeetingURL(t *testing.T) {
	h := &handler{}
	for _, id := range []string{"join-file:///etc/passwd", "join-https://evil.example/l/meetup-join/x", "join-https://teams.microsoft.com/l/meetup-join/x /Applications/Calculator.app"} {
		if ok, msg := h.PerformAction(id, nil); ok || msg != "invalid meeting URL" {
			t.Fatalf("%s: ok=%v msg=%q", id, ok, msg)
		}
	}
}

func TestTruncKeepsUTF8Valid(t *testing.T) {
	s := strings.Repeat("é", 100)
	if got := trunc(s, 81); !utf8.ValidString(got) {
		t.Fatalf("invalid utf8: %q", got)
	}
}
