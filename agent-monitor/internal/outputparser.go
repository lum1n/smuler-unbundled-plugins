package internal

import "strings"

type OutputInsight struct {
	HasBlocker    bool   `json:"hasBlocker"`
	BlockerKind   string `json:"blockerKind,omitempty"`
	HasErrorLoop  bool   `json:"hasErrorLoop"`
	ErrorLoopLine string `json:"errorLoopLine,omitempty"`
	IsComplete    bool   `json:"isComplete"`
	ProgressPct   int    `json:"progressPct"`
}

var blockerPatterns = []struct {
	keyword string
	kind    string
}{
	{"permission denied", "permission"},
	{"command not found", "missing_command"},
	{"no such file or directory", "missing_file"},
	{"cannot find module", "missing_module"},
	{"import cycle", "import_cycle"},
	{"out of memory", "oom"},
	{"killed", "killed"},
	{"connection refused", "network"},
	{"unauthorized", "auth"},
	{"authentication required", "auth"},
	{"access denied", "auth"},
	{"rate limit", "rate_limit"},
	{"too many requests", "rate_limit"},
	{"api key", "auth"},
	{"invalid credentials", "auth"},
}

var completionSignals = []string{
	"task completed",
	"done.",
	"done!",
	"successfully",
	"all tests pass",
	"no errors",
	"everything looks good",
}

func ParseOutput(outputTail string) OutputInsight {
	if outputTail == "" {
		return OutputInsight{}
	}

	lower := strings.ToLower(outputTail)
	insight := OutputInsight{}

	lineCounts := make(map[string]int)
	lines := strings.Split(lower, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, bp := range blockerPatterns {
			if strings.Contains(line, bp.keyword) {
				insight.HasBlocker = true
				insight.BlockerKind = bp.kind
				break
			}
		}
		if strings.Contains(line, "error:") ||
			strings.Contains(line, "error ") ||
			strings.HasPrefix(line, "error") {
			lineCounts[strings.TrimSpace(line[:min(60, len(line))])]++
		}
	}

	for line, count := range lineCounts {
		if count >= 3 {
			insight.HasErrorLoop = true
			insight.ErrorLoopLine = line
			break
		}
	}

	for _, signal := range completionSignals {
		if strings.Contains(lower, signal) {
			insight.IsComplete = true
			break
		}
	}

	return insight
}
