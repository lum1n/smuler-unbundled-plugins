package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lum1n/smuler/plugins/sdk-go"
)

const (
	pluginID      = "teams"
	pluginVersion = "0.1.0"
)

var homeDir, _ = os.UserHomeDir()

func findLogPath() (string, string) {
	// Try the direct Teams data directory first
	teamsDir := filepath.Join(homeDir, "Library/Application Support/Microsoft/Teams")
	if path, errMsg := tryTeamsDir(teamsDir); path != "" {
		return path, ""
	} else if errMsg != "" {
		// Directory exists but couldn't find logs — return diagnostic
		return "", errMsg
	}

	// Search under Teams containers (handles sandboxed and New Teams)
	containersDir := filepath.Join(homeDir, "Library/Containers")
	entries, err := os.ReadDir(containersDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "com.microsoft.teams") {
				continue
			}
			teamsData := filepath.Join(containersDir, e.Name(), "Data/Library/Application Support/Microsoft/Teams")
			if path := findLogFile(teamsData); path != "" {
				return path, ""
			}
			// Also try the container Data root for flat log files
			dataRoot := filepath.Join(containersDir, e.Name(), "Data")
			if path := findLogFile(dataRoot); path != "" {
				return path, ""
			}
		}
	}

	// Search more broadly under Application Support for any Microsoft Teams dirs
	appSupport := filepath.Join(homeDir, "Library/Application Support")
	if entries, err := os.ReadDir(appSupport); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() || !strings.Contains(strings.ToLower(name), "teams") {
				continue
			}
			if path := findLogFile(filepath.Join(appSupport, name)); path != "" {
				return path, ""
			}
		}
		// Also check Microsoft directory for Teams subdirectories
		microsoftDir := filepath.Join(appSupport, "Microsoft")
		if entries, err := os.ReadDir(microsoftDir); err == nil {
			for _, e := range entries {
				name := e.Name()
				if !e.IsDir() || !strings.Contains(strings.ToLower(name), "teams") {
					continue
				}
				if path := findLogFile(filepath.Join(microsoftDir, name)); path != "" {
					return path, ""
				}
			}
		}
	}

	// Search in Library/Logs for Teams log files
	logsDir := filepath.Join(homeDir, "Library/Logs")
	if entries, err := os.ReadDir(logsDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.Contains(strings.ToLower(name), "teams") {
				if !e.IsDir() || name != "Microsoft" {
					continue
				}
			}
			if e.IsDir() {
				if path := findLogFile(filepath.Join(logsDir, name)); path != "" {
					return path, ""
				}
			} else if fi, err := e.Info(); err == nil && !fi.IsDir() {
				if strings.Contains(strings.ToLower(name), "log") || strings.HasSuffix(name, ".txt") {
					return filepath.Join(logsDir, name), ""
				}
			}
		}
		// Specifically check Microsoft subdir in Logs
		if path := findLogFile(filepath.Join(logsDir, "Microsoft")); path != "" {
			return path, ""
		}
	}

	// Search in Library/Caches for Teams log files (Electron apps log here)
	cachesDir := filepath.Join(homeDir, "Library/Caches")
	if entries, err := os.ReadDir(cachesDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || !strings.Contains(strings.ToLower(e.Name()), "teams") {
				continue
			}
			if path := findLogFile(filepath.Join(cachesDir, e.Name())); path != "" {
				return path, ""
			}
		}
	}

	return "", "no Teams log file found in any known location"
}

func tryTeamsDir(dir string) (string, string) {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "" // Dir doesn't exist — not an error, try elsewhere
		}
		return "", "cannot access Teams dir: " + err.Error()
	}
	if !dirInfo.IsDir() {
		return "", "Teams path is not a directory: " + dir
	}

	candidates := []string{
		filepath.Join(dir, "logs.txt"),
		filepath.Join(dir, "logs"),
	}
	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, ""
		}
	}

	// Check known log subdirectories
	logSubdirs := []string{"logs", "log", "Logs"}
	for _, sub := range logSubdirs {
		subPath := filepath.Join(dir, sub)
		if st, err := os.Stat(subPath); err == nil && st.IsDir() {
			if path := findLogFile(subPath); path != "" {
				return path, ""
			}
		}
	}

	if path := findLogFile(dir); path != "" {
		return path, ""
	}

	contents, _ := os.ReadDir(dir)
	names := make([]string, 0, len(contents))
	for _, e := range contents {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", "Teams data dir is empty or unreadable"
	}
	return "", "no log file found. Dir contents: " + strings.Join(names, ", ")
}

func findLogFile(dir string) string {
	// First try the find command for speed on large dirs. Bounded so a huge
	// container tree cannot stall the initialize handshake.
	ctx, cancel := context.WithTimeout(context.Background(), findTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "find", dir, "-maxdepth", "5", "-type", "f",
		"(", "-name", "*log*", "-o", "-name", "*.txt", "-o", "-name", "*.log", ")").Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" && !isLevelDBFile(line) {
				return line
			}
		}
		return ""
	}
	if ctx.Err() != nil {
		return ""
	}

	// Fallback: walk manually, skip LevelDB companion files
	var found string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if found != "" {
			return filepath.SkipAll
		}
		if err != nil {
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if isLevelDBFile(path) {
			return nil
		}
		name := strings.ToLower(info.Name())
		if strings.Contains(name, "log") || strings.HasSuffix(name, ".txt") || strings.HasSuffix(name, ".log") {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// findTimeout bounds each external `find` used for log discovery.
const findTimeout = 5 * time.Second

func isLevelDBFile(path string) bool {
	name := filepath.Base(path)
	return strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-wal") ||
		strings.HasSuffix(name, ".ldb") || strings.HasSuffix(name, ".sst")
}

var (
	reMention   = regexp.MustCompile(`(?i)@mention["\s:=]`)
	reMeetupURL = regexp.MustCompile(`(msteams://teams\.microsoft\.com/l/meetup-join/[^\s"']+|https://teams\.microsoft\.com/l/meetup-join/[^\s"']+)`)
	reCallState = regexp.MustCompile(`(?i)("callState"|call_state|incomingCall|callRinging|CallEstablished|CallEnded|callConnected)`)
	reChatMsg   = regexp.MustCompile(`(?i)notification.*("text"\s*:\s*"([^"]+)".*|eventType.*chat)`)
	reTimestamp = regexp.MustCompile(`(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2})`)
)

type handler struct {
	maxLogLines  int
	refreshSec   int
	lastKnownURL string
	logPath      string
	logErr       string

	// Log stream fallback for New Teams (no log file on disk)
	logStreamMu     sync.Mutex
	logStreamBuf    []string
	logStreamCancel context.CancelFunc
}

func (h *handler) Initialize(params sdk.InitializeParams) string {
	h.maxLogLines = 2000
	h.refreshSec = 10
	h.logPath, h.logErr = findLogPath()
	if v, ok := params.Config["maxLogLines"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			h.maxLogLines = n
		}
	}
	if v, ok := params.Config["refreshSeconds"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			h.refreshSec = n
		}
	}

	// If no log file found, start log stream for New Teams
	if h.logPath == "" {
		h.startLogStream()
	}
	return sdk.HealthOK
}

func (h *handler) GetStatus() sdk.Snapshot {
	teamsRunning := isTeamsRunning()

	if !teamsRunning {
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateReady,
			Summary: sdk.Summary{
				Title:    "Teams",
				Value:    "Not running",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityInfo,
				IconHint: "person.2",
			},
			Items: []sdk.Item{{
				ID:       "teams-not-running",
				Title:    "Microsoft Teams is not running",
				Subtitle: "Launch Teams to see mentions and meetings",
				Severity: sdk.SeverityInfo,
				Actions:  []sdk.Action{{ID: "launch", Label: "Launch Teams"}},
			}},
			RefreshAfter: h.refreshSec,
			Health:       sdk.HealthOK,
		}
	}

	if h.logPath == "" {
		// Try log stream data (New Teams)
		lines := h.readLogStream()
		if len(lines) > 0 {
			return h.buildSnapshot(lines)
		}
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateReady,
			Summary: sdk.Summary{
				Title:    "Teams",
				Value:    "Running",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityInfo,
				IconHint: "person.2",
			},
			Items: []sdk.Item{{
				ID:       "teams-no-log",
				Title:    "Listening for activity",
				Subtitle: "Teams is running. Watching system log for mentions, calls, and meetings.",
				Severity: sdk.SeverityInfo,
				Actions:  []sdk.Action{{ID: "launch", Label: "Open Teams"}},
			}},
			RefreshAfter: h.refreshSec,
			Health:       sdk.HealthOK,
		}
	}

	logLines, err := tailLog(h.logPath, h.maxLogLines)
	if err != nil {
		errMsg := err.Error()
		return sdk.Snapshot{
			PluginID: pluginID,
			State:    sdk.StateError,
			Summary: sdk.Summary{
				Title:    "Teams",
				Value:    "Log read error",
				Trend:    sdk.TrendSteady,
				Severity: sdk.SeverityWarning,
				IconHint: "person.2",
			},
			Items: []sdk.Item{{
				ID:       "teams-log-error",
				Title:    "Cannot read Teams log",
				Subtitle: errMsg,
				Severity: sdk.SeverityWarning,
				Actions:  []sdk.Action{},
			}},
			RefreshAfter: 30,
			Health:       sdk.HealthDegraded,
		}
	}

	return h.buildSnapshot(logLines)
}

func (h *handler) buildSnapshot(logLines []string) sdk.Snapshot {
	events := parseEvents(logLines)
	items := buildItems(events, h)

	if h.lastKnownURL == "" {
		for _, e := range events {
			if e.meetupURL != "" {
				h.lastKnownURL = e.meetupURL
				break
			}
		}
	}

	mentionCount := 0
	meetingCount := 0
	inCall := false
	for _, e := range events {
		if e.isMention {
			mentionCount++
		}
		if e.meetupURL != "" || e.isMeeting {
			meetingCount++
		}
		if e.callState == "ringing" || e.callState == "connected" {
			inCall = true
		}
	}

	summaryValue := "Idle"
	severity := sdk.SeverityInfo
	if inCall {
		summaryValue = "In call"
		severity = sdk.SeverityInfo
	} else if mentionCount > 0 {
		summaryValue = fmt.Sprintf("%d mentions", mentionCount)
		severity = sdk.SeverityWarning
	}
	if meetingCount > 0 {
		summaryValue += fmt.Sprintf(" · %d meetings", meetingCount)
	}

	alerts := []sdk.Alert{}
	if mentionCount > 0 {
		alerts = append(alerts, sdk.Alert{
			ID:       "mentions",
			Severity: sdk.SeverityWarning,
			Message:  fmt.Sprintf("%d mention(s) in Teams", mentionCount),
		})
	}
	if inCall {
		alerts = append(alerts, sdk.Alert{
			ID:       "in-call",
			Severity: sdk.SeverityInfo,
			Message:  "You are in a Teams call",
		})
	}

	if len(items) == 0 {
		items = append(items, sdk.Item{
			ID:       "no-events",
			Title:    "No recent activity",
			Subtitle: "Teams is running but no mentions or meetings detected",
			Severity: sdk.SeverityInfo,
			Actions:  []sdk.Action{},
		})
	}

	return sdk.Snapshot{
		PluginID: pluginID,
		State:    sdk.StateReady,
		Summary: sdk.Summary{
			Title:    "Teams",
			Value:    summaryValue,
			Trend:    sdk.TrendSteady,
			Severity: severity,
			IconHint: "person.2",
		},
		Items:        items,
		Actions:      []sdk.Action{{ID: "open-teams", Label: "Open Teams"}},
		Alerts:       alerts,
		RefreshAfter: h.refreshSec,
		Health:       sdk.HealthOK,
	}
}

func (h *handler) PerformAction(id string, params map[string]string) (bool, string) {
	switch {
	case id == "launch" || id == "open-teams":
		if err := exec.Command("open", "-a", "Microsoft Teams").Run(); err != nil {
			return false, fmt.Sprintf("failed to launch Teams: %v", err)
		}
		return true, ""

	case strings.HasPrefix(id, "join-"):
		url := strings.TrimPrefix(id, "join-")
		if url == "" {
			return false, "no meeting URL"
		}
		// Only ever hand Teams meeting links to `open`; the action id is
		// caller-supplied and must not become an arbitrary URL/file opener.
		if reMeetupURL.FindString(url) != url {
			return false, "invalid meeting URL"
		}
		if err := exec.Command("open", url).Run(); err != nil {
			return false, fmt.Sprintf("failed to join: %v", err)
		}
		return true, ""

	case id == "dismiss":
		return true, ""

	default:
		return false, "unknown action: " + id
	}
}

func (h *handler) Shutdown() {
	h.stopLogStream()
}

// startLogStream spawns log stream to capture system log entries from the
// Teams process. Used as fallback when no log file exists (New Teams).
func (h *handler) startLogStream() {
	ctx, cancel := context.WithCancel(context.Background())
	h.logStreamCancel = cancel

	go func() {
		cmd := exec.CommandContext(ctx, "log", "stream",
			"--predicate", `process CONTAINS[c] "teams"`,
			"--style", "compact",
		)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return
		}
		if err := cmd.Start(); err != nil {
			return
		}

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(nil, 512*1024)
		for scanner.Scan() {
			line := scanner.Text()
			h.appendLogLine(line)
		}
		cmd.Wait()
	}()
}

func (h *handler) stopLogStream() {
	if h.logStreamCancel != nil {
		h.logStreamCancel()
		h.logStreamCancel = nil
	}
}

func (h *handler) appendLogLine(line string) {
	h.logStreamMu.Lock()
	defer h.logStreamMu.Unlock()

	// Ring buffer: keep last 2000 lines
	const maxBuf = 2000
	h.logStreamBuf = append(h.logStreamBuf, line)
	if len(h.logStreamBuf) > maxBuf {
		h.logStreamBuf = h.logStreamBuf[len(h.logStreamBuf)-maxBuf:]
	}
}

func (h *handler) readLogStream() []string {
	h.logStreamMu.Lock()
	defer h.logStreamMu.Unlock()

	// Return lines from the last ~30s
	cutoff := time.Now().Add(-30 * time.Second)
	var recent []string
	for i := len(h.logStreamBuf) - 1; i >= 0; i-- {
		line := h.logStreamBuf[i]
		ts := extractLogStreamTimestamp(line)
		if ts.IsZero() || ts.After(cutoff) {
			recent = append(recent, line)
		} else if !ts.IsZero() {
			break // Lines are in order; stop when past cutoff
		}
	}
	// Reverse to chronological order
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return recent
}

// log stream timestamps are local wall-clock time, e.g.
// "2026-06-30 14:20:45.123456+0200" (default) or "2026-06-30 14:20:45.123" (compact).
var reLogStreamTS = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+)`)

func extractLogStreamTimestamp(line string) time.Time {
	m := reLogStreamTS.FindStringSubmatch(line)
	if len(m) < 2 {
		return time.Time{}
	}
	// Variable-width fraction (compact style prints milliseconds) and local
	// zone: parsing as UTC shifted every line by the UTC offset, so the 30s
	// window either kept stale lines or dropped all of them.
	t, _ := time.ParseInLocation("2006-01-02 15:04:05.999999999", m[1], time.Local)
	return t
}

type logEvent struct {
	id        string
	timestamp string
	message   string
	isMention bool
	meetupURL string
	isMeeting bool
	callState string
	chatText  string
}

func buildItems(events []logEvent, h *handler) []sdk.Item {
	items := make([]sdk.Item, 0, len(events))
	for _, e := range events {
		switch {
		case e.callState == "ringing":
			items = append(items, sdk.Item{
				ID:        "call-ringing-" + e.id,
				Title:     "Incoming call",
				Subtitle:  "Teams call ringing",
				Severity:  sdk.SeverityWarning,
				Timestamp: e.timestamp,
				Actions:   []sdk.Action{{ID: "open-teams", Label: "Answer in Teams"}},
			})
		case e.callState == "connected":
			items = append(items, sdk.Item{
				ID:        "call-active-" + e.id,
				Title:     "In call",
				Subtitle:  "Call in progress",
				Severity:  sdk.SeverityInfo,
				Timestamp: e.timestamp,
				Actions:   []sdk.Action{{ID: "open-teams", Label: "Open Teams"}},
			})
		case e.meetupURL != "":
			title := "Meeting available"
			if e.chatText != "" {
				title = trunc(e.chatText, 80)
			}
			items = append(items, sdk.Item{
				ID:        "meeting-" + e.id,
				Title:     title,
				Subtitle:  "Click Join to enter",
				Severity:  sdk.SeverityInfo,
				Timestamp: e.timestamp,
				Actions: []sdk.Action{
					{ID: "join-" + e.meetupURL, Label: "Join"},
					{ID: "dismiss", Label: "Dismiss"},
				},
			})
		case e.isMention:
			title := "You were mentioned"
			if e.chatText != "" {
				title = trunc(e.chatText, 80)
			}
			items = append(items, sdk.Item{
				ID:        "mention-" + e.id,
				Title:     title,
				Subtitle:  "@mention in Teams",
				Severity:  sdk.SeverityWarning,
				Timestamp: e.timestamp,
				Actions: []sdk.Action{
					{ID: "open-teams", Label: "Open Teams"},
					{ID: "dismiss", Label: "Dismiss"},
				},
			})
		case e.isMeeting:
			items = append(items, sdk.Item{
				ID:        "meeting-start-" + e.id,
				Title:     "Meeting started",
				Subtitle:  e.chatText,
				Severity:  sdk.SeverityInfo,
				Timestamp: e.timestamp,
				Actions:   []sdk.Action{{ID: "open-teams", Label: "Open Teams"}},
			})
		case e.chatText != "":
			items = append(items, sdk.Item{
				ID:        "msg-" + e.id,
				Title:     trunc(e.chatText, 80),
				Subtitle:  "New message",
				Severity:  sdk.SeverityInfo,
				Timestamp: e.timestamp,
				Actions:   []sdk.Action{{ID: "open-teams", Label: "Open Teams"}},
			})
		}
	}
	return items
}

func isTeamsRunning() bool {
	out, err := exec.Command("pgrep", "-f", "Microsoft Teams").Output()
	if err != nil {
		return false
	}
	for _, s := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(s); err == nil && pid != os.Getpid() {
			return true
		}
	}
	return false
}

func tailLog(logPath string, maxLines int) ([]string, error) {
	if logPath == "" {
		return nil, fmt.Errorf("no Teams log file found")
	}
	file, err := os.Open(logPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	defer file.Close()

	// Read backwards from EOF in chunks until we have maxLines lines, instead
	// of scanning the whole (often hundreds of MB) log on every refresh.
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	const chunk = 64 * 1024
	maxBytes := int64(maxLines) * 4096 // bound pathological long-line logs
	end := st.Size()
	var buf []byte
	pos := end
	for pos > 0 && end-pos < maxBytes && bytes.Count(buf, []byte{'\n'}) <= maxLines {
		n := int64(chunk)
		if pos < n {
			n = pos
		}
		pos -= n
		part := make([]byte, n)
		if _, err := file.ReadAt(part, pos); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(part, buf...)
	}
	if pos > 0 {
		// Drop the partial first line.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		} else {
			buf = nil
		}
	}

	text := strings.TrimRight(string(buf), "\r\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}

func parseEvents(lines []string) []logEvent {
	events := make([]logEvent, 0, 20)
	seen := make(map[string]bool)

	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		var event logEvent
		event.timestamp = extractTimestamp(line)
		event.id = lineID(line)

		if reMention.MatchString(line) {
			event.isMention = true
			if text := extractText(line); text != "" {
				event.chatText = text
			}
		}

		if m := reMeetupURL.FindString(line); m != "" {
			event.meetupURL = m
		}

		if reCallState.MatchString(line) {
			lower := strings.ToLower(line)
			switch {
			case strings.Contains(lower, "incomingcall") || strings.Contains(lower, "callringing") || strings.Contains(lower, "ringing"):
				event.callState = "ringing"
			case strings.Contains(lower, "callestablished") || strings.Contains(lower, "callconnected"):
				event.callState = "connected"
			case strings.Contains(lower, "callended"):
				event.callState = "ended"
			}
		}

		if !event.isMention && event.meetupURL == "" && event.callState == "" {
			if text := extractText(line); text != "" && strings.Contains(line, "notification") {
				event.chatText = text
			}
		}

		if event.isMention || event.meetupURL != "" || event.callState != "" || event.chatText != "" {
			key := event.timestamp + fmt.Sprintf("%v%v%v%v", event.isMention, event.meetupURL, event.callState, event.chatText)
			if event.timestamp == "" {
				key = event.id
			}
			if !seen[key] {
				seen[key] = true
				events = append(events, event)
			}
		}

		if len(events) >= 15 {
			break
		}
	}

	return events
}

// extractTimestamp returns the line's timestamp as RFC 3339 (the host expects
// ISO 8601), interpreting the zone-less log stamp as local time. Lines without
// a stamp get "" rather than "now", which made every refresh look new.
func extractTimestamp(line string) string {
	m := reTimestamp.FindStringSubmatch(line)
	if len(m) < 2 {
		return ""
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.Replace(m[1], "T", " ", 1), time.Local)
	if err != nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

// lineID derives a stable item id from the raw log line so the same event
// keeps its id across refreshes (and distinct events in the same second differ).
func lineID(line string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(line))
	return strconv.FormatUint(h.Sum64(), 16)
}

func extractText(line string) string {
	start := strings.Index(line, `"text"`)
	if start < 0 {
		return ""
	}
	colon := strings.Index(line[start:], ":")
	if colon < 0 {
		return ""
	}
	rest := line[start+colon+1:]
	rest = strings.TrimSpace(rest)
	rest = strings.Trim(rest, `"`)
	if strings.HasPrefix(rest, "\\\"") || strings.HasPrefix(rest, `\"`) {
		rest = rest[2:]
	}
	return truncBytes(rest, 200)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncBytes(s, n) + "..."
}

// truncBytes cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func main() {
	sdk.Run(pluginID, pluginVersion, &handler{})
}
