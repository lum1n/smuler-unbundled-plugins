package sdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureOutput redirects SDK stdout writes into a buffer for the test.
func captureOutput(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	outMu.Lock()
	prev := out
	out = buf
	outMu.Unlock()
	t.Cleanup(func() {
		outMu.Lock()
		out = prev
		outMu.Unlock()
	})
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) lines(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid JSON output line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func responseIDs(lines []map[string]json.RawMessage) []string {
	var ids []string
	for _, l := range lines {
		if id, ok := l["id"]; ok {
			ids = append(ids, string(id))
		}
	}
	return ids
}

func TestServeHandlesInitializeLargerThanScannerDefault(t *testing.T) {
	buf := captureOutput(t)
	h := &testHandler{}

	peers := make([]Snapshot, 0, 3000)
	for i := 0; i < 3000; i++ {
		peers = append(peers, Snapshot{
			PluginID: fmt.Sprintf("peer-%d", i),
			Summary:  Summary{Title: strings.Repeat("x", 1024)},
		})
	}
	params, err := json.Marshal(InitializeParams{ProtocolVersion: "0.1.0", PluginID: "test", PeerSnapshots: peers})
	if err != nil {
		t.Fatal(err)
	}
	if len(params) < 2*1024*1024 {
		t.Fatalf("test payload too small: %d bytes", len(params))
	}
	input := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":%s}`+"\n"+
		`{"jsonrpc":"2.0","id":2,"method":"getStatus","params":{}}`+"\n", params)

	serve(strings.NewReader(input), "1.0.0", h)

	if !h.initCalled || len(h.initParams.PeerSnapshots) != 3000 {
		t.Fatalf("initialize not delivered intact (called=%v peers=%d)", h.initCalled, len(h.initParams.PeerSnapshots))
	}
	ids := responseIDs(buf.lines(t))
	if len(ids) != 2 || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("response ids = %v, want [1 2]", ids)
	}
}

func TestServeEchoesStringIDsAndSkipsNotifications(t *testing.T) {
	buf := captureOutput(t)
	input := `{"jsonrpc":"2.0","id":"abc","method":"getStatus"}` + "\n" +
		`{"jsonrpc":"2.0","method":"refresh"}` + "\n" +
		`{"jsonrpc":"2.0","method":"bogus"}` + "\n" +
		`{"jsonrpc":"2.0","id":7,"method":"bogus"}` + "\n"
	serve(strings.NewReader(input), "1.0.0", &testHandler{})

	lines := buf.lines(t)
	ids := responseIDs(lines)
	if len(ids) != 2 {
		t.Fatalf("expected 2 responses, got %d: %v", len(ids), ids)
	}
	got := map[string]bool{ids[0]: true, ids[1]: true}
	if !got[`"abc"`] || !got["7"] {
		t.Fatalf("response ids = %v, want \"abc\" and 7", ids)
	}
}

type blockingHandler struct {
	testHandler
	release chan struct{}
}

func (h *blockingHandler) GetStatus() Snapshot {
	<-h.release
	return h.testHandler.GetStatus()
}

func TestShutdownNotBlockedBySlowRefresh(t *testing.T) {
	buf := captureOutput(t)
	prevGrace := shutdownGrace
	shutdownGrace = 50 * time.Millisecond
	t.Cleanup(func() { shutdownGrace = prevGrace })

	h := &blockingHandler{release: make(chan struct{})}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		serve(pr, "1.0.0", h)
		close(done)
	}()
	_, _ = io.WriteString(pw, `{"jsonrpc":"2.0","id":1,"method":"refresh"}`+"\n")
	_, _ = io.WriteString(pw, `{"jsonrpc":"2.0","id":2,"method":"shutdown"}`+"\n")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked behind slow refresh")
	}
	_ = pw.Close()

	ids := responseIDs(buf.lines(t))
	if len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("response ids = %v, want [2]", ids)
	}

	// Unblock the stuck refresh and wait for its late response so it does not
	// leak into another test's captured output.
	close(h.release)
	deadline := time.Now().Add(2 * time.Second)
	for len(responseIDs(buf.lines(t))) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConcurrentStatusRequestsShareOneComputation(t *testing.T) {
	buf := captureOutput(t)
	h := &countingHandler{release: make(chan struct{})}

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		serve(pr, "1.0.0", h)
		close(done)
	}()
	for i := 1; i <= 3; i++ {
		_, _ = fmt.Fprintf(pw, `{"jsonrpc":"2.0","id":%d,"method":"refresh"}`+"\n", i)
	}
	// Allow the loop to enqueue all three before the first computation ends.
	time.Sleep(50 * time.Millisecond)
	close(h.release)
	_ = pw.Close()
	<-done

	ids := responseIDs(buf.lines(t))
	if len(ids) != 3 {
		t.Fatalf("expected 3 responses, got %v", ids)
	}
	if h.calls != 1 {
		t.Fatalf("GetStatus called %d times, want 1", h.calls)
	}
}

type countingHandler struct {
	testHandler
	release chan struct{}
	calls   int
}

func (h *countingHandler) GetStatus() Snapshot {
	h.calls++
	<-h.release
	Emit(Event{Type: "tick", Message: "hello"})
	return h.testHandler.GetStatus()
}

func TestPanicInHandlerReturnsErrorAndKeepsServing(t *testing.T) {
	buf := captureOutput(t)
	input := `{"jsonrpc":"2.0","id":1,"method":"performAction","params":{"actionId":"boom"}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"getStatus"}` + "\n"
	serve(strings.NewReader(input), "1.0.0", &panickyHandler{})

	lines := buf.lines(t)
	var sawErr, sawStatus bool
	for _, l := range lines {
		switch string(l["id"]) {
		case "1":
			_, sawErr = l["error"]
		case "2":
			_, sawStatus = l["result"]
		}
	}
	if !sawErr || !sawStatus {
		t.Fatalf("expected error for 1 and result for 2, got %d lines", len(lines))
	}
}

type panickyHandler struct{ testHandler }

func (h *panickyHandler) PerformAction(id string, params map[string]string) (bool, string) {
	panic("boom")
}

func (h *testHandler) PerformAction(id string, params map[string]string) (bool, string) {
	return true, ""
}

func TestReadLineDropsOversizedLineAndContinues(t *testing.T) {
	input := strings.Repeat("a", 100) + "\nshort\nlast"
	r := bufio.NewReaderSize(strings.NewReader(input), 16)

	if _, err := readLine(r, 32); err != errLineTooLong {
		t.Fatalf("first line err = %v, want errLineTooLong", err)
	}
	line, err := readLine(r, 32)
	if err != nil || string(line) != "short\n" {
		t.Fatalf("second line = %q, %v", line, err)
	}
	line, err = readLine(r, 32)
	if err != nil || string(line) != "last" {
		t.Fatalf("third line = %q, %v", line, err)
	}
	if _, err := readLine(r, 32); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}
