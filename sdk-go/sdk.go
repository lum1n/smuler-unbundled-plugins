package sdk

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lum1n/smuler/plugins/plugindebug"
)

// Handler is the interface a plugin must implement.
type Handler interface {
	// PerformAction is called when the host sends a "performAction" request.
	// Return true + optional data on success, or false + error message on failure.
	// Prefer ActionResultHandler when returning structured window content.
	PerformAction(id string, params map[string]string) (bool, string)
	// Initialize is called when the host sends the "initialize" handshake.
	// Return the initial health string (e.g. HealthOK).
	Initialize(params InitializeParams) string

	// GetStatus is called for both "getStatus" and "refresh" requests.
	// Return the current plugin snapshot. Call Emit() during this method
	// to send unsolicited event notifications.
	GetStatus() Snapshot

	// Shutdown is called when the host requests graceful shutdown.
	Shutdown()
}

// ActionResultHandler is an optional extension for plugins that return
// structured ActionResult values (including host-rendered windows).
// When implemented, it takes precedence over Handler.PerformAction.
type ActionResultHandler interface {
	PerformActionResult(id string, params map[string]string) ActionResult
}

// --- package-level API for plugin authors ---

// Log writes a debug message to stderr with a "[<pluginID>]" prefix when debug logging is enabled.
func Log(format string, args ...interface{}) {
	if !plugindebug.Enabled() {
		return
	}
	prefix := "[plugin]"
	if currentPluginID != "" {
		prefix = "[" + currentPluginID + "]"
	}
	plugindebug.Log(prefix, format, args...)
}

// Emit sends an unsolicited event notification to the host.
// Call this from your Handler.GetStatus() implementation. Safe for concurrent use.
func Emit(event Event) {
	if currentPluginID != "" {
		event.PluginID = currentPluginID
	}
	event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: "event", Params: eventParams{Event: event}})
	if err != nil {
		Log("json marshal error in Emit: %v", err)
		return
	}
	writeLine(data)
}

// --- internal ---

var currentPluginID string

// maxLineBytes bounds a single inbound JSON-RPC line. Initialize payloads can
// carry peer snapshots, so this is far above bufio.Scanner's 64 KiB default.
const maxLineBytes = 64 * 1024 * 1024

// shutdownGrace bounds how long "shutdown" waits for in-flight handler calls
// before replying anyway (the host only waits ~2s for the shutdown response).
var shutdownGrace = 1500 * time.Millisecond

var errLineTooLong = errors.New("json-rpc line exceeds limit")

// stdout is shared by responses and Emit; every line is written atomically
// under outMu so concurrent writers never interleave.
var (
	outMu sync.Mutex
	out   io.Writer = os.Stdout
)

func writeLine(data []byte) {
	line := make([]byte, 0, len(data)+1)
	line = append(line, data...)
	line = append(line, '\n')
	outMu.Lock()
	defer outMu.Unlock()
	if _, err := out.Write(line); err != nil {
		Log("stdout write error: %v", err)
	}
}

type eventParams struct {
	Event Event `json:"event"`
}

type rpcNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

// rpcRequest keeps the id raw so numeric and string ids are echoed back
// verbatim; a missing or null id marks a notification (no response).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (r rpcRequest) hasID() bool {
	return len(r.ID) > 0 && string(r.ID) != "null"
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    *pluginErrorData `json:"data,omitempty"`
}

type pluginErrorData struct {
	Retryable       bool   `json:"retryable"`
	SuggestedAction string `json:"suggestedAction,omitempty"`
}

type initPayload struct {
	Type            string `json:"type"`
	ProtocolVersion string `json:"protocolVersion"`
	PluginVersion   string `json:"pluginVersion"`
	Health          string `json:"health"`
}

func sendResult(id json.RawMessage, result interface{}) {
	data, err := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
	if err != nil {
		Log("json marshal error: %v", err)
		sendError(id, -32603, "failed to encode result", true, "")
		return
	}
	writeLine(data)
}

func sendError(id json.RawMessage, code int, message string, retryable bool, suggestedAction string) {
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
			Data:    &pluginErrorData{Retryable: retryable, SuggestedAction: suggestedAction},
		},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		Log("json marshal error: %v", err)
		return
	}
	writeLine(data)
}

// readLine returns the next newline-terminated line (without the newline).
// Lines longer than max are discarded and reported as errLineTooLong so the
// loop can keep serving subsequent requests instead of dying.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch err {
		case nil:
			if tooLong {
				return nil, errLineTooLong
			}
			return buf, nil
		case bufio.ErrBufferFull:
			continue
		default:
			if tooLong {
				return nil, errLineTooLong
			}
			if len(buf) > 0 && err == io.EOF {
				return buf, nil
			}
			return nil, err
		}
	}
}

// server serializes calls into the (non-thread-safe) Handler while the stdin
// loop keeps reading, so a slow refresh never blocks shutdown, and concurrent
// getStatus/refresh requests share one in-flight snapshot computation.
type server struct {
	handler   Handler
	handlerMu sync.Mutex
	initResp  initPayload

	flightMu sync.Mutex
	flight   *statusFlight
	wg       sync.WaitGroup
}

type statusFlight struct {
	ids []json.RawMessage
}

// call runs fn under the handler lock, converting a panic into an error.
func (s *server) call(method string, fn func()) (err error) {
	s.handlerMu.Lock()
	defer s.handlerMu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			Log("panic in %s: %v", method, r)
			err = fmt.Errorf("plugin panic in %s", method)
		}
	}()
	fn()
	return nil
}

func (s *server) handleStatus(req rpcRequest) {
	if !req.hasID() {
		return
	}
	s.flightMu.Lock()
	if s.flight != nil {
		s.flight.ids = append(s.flight.ids, req.ID)
		s.flightMu.Unlock()
		return
	}
	f := &statusFlight{ids: []json.RawMessage{req.ID}}
	s.flight = f
	s.flightMu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var snap Snapshot
		err := s.call(req.Method, func() { snap = s.handler.GetStatus() })

		s.flightMu.Lock()
		ids := f.ids
		s.flight = nil
		s.flightMu.Unlock()

		for _, id := range ids {
			if err != nil {
				sendError(id, -32603, err.Error(), true, "")
			} else {
				sendResult(id, snap)
			}
		}
	}()
}

func (s *server) handleAction(req rpcRequest) {
	actionID, payload := parsePerformActionParams(req.Params)
	if actionID == "" {
		if req.hasID() {
			sendError(req.ID, -32602, "performAction missing actionId", false, "Retry the action.")
		}
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		var result ActionResult
		err := s.call(req.Method, func() {
			if richer, ok := s.handler.(ActionResultHandler); ok {
				result = richer.PerformActionResult(actionID, payload)
			} else {
				success, data := s.handler.PerformAction(actionID, payload)
				result = ActionResult{Success: success, Data: data}
				if !success {
					result.Error = data
				}
			}
		})
		if !req.hasID() {
			return
		}
		if err != nil {
			sendError(req.ID, -32603, err.Error(), true, "")
			return
		}
		sendResult(req.ID, result)
	}()
}

// handleShutdown calls Handler.Shutdown once in-flight work finishes, but
// replies within shutdownGrace even if a handler call is stuck.
func (s *server) handleShutdown(req rpcRequest) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.call(req.Method, s.handler.Shutdown)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		Log("shutdown: handler busy, replying without waiting")
	}
	if req.hasID() {
		sendResult(req.ID, nil)
	}
}

// Run starts the JSON-RPC stdio loop. It blocks until the plugin receives
// "shutdown" or stdin closes. pluginID and pluginVersion are sent to the
// host during the initialize handshake.
func Run(pluginID, pluginVersion string, handler Handler) {
	currentPluginID = pluginID
	serve(os.Stdin, pluginVersion, handler)
}

func serve(in io.Reader, pluginVersion string, handler Handler) {
	s := &server{
		handler: handler,
		initResp: initPayload{
			Type:            "initialized",
			ProtocolVersion: "0.1.0",
			PluginVersion:   pluginVersion,
			Health:          HealthOK,
		},
	}

	reader := bufio.NewReaderSize(in, 64*1024)
	for {
		raw, err := readLine(reader, maxLineBytes)
		if err == errLineTooLong {
			Log("dropping oversized request line (> %d bytes)", maxLineBytes)
			continue
		}
		if err != nil {
			if err != io.EOF {
				Log("stdin read error: %v", err)
			}
			break
		}
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			Log("dropping malformed request: %v", err)
			continue
		}

		switch req.Method {
		case "initialize":
			// Handled inline so later requests observe initialized state.
			var params InitializeParams
			if err := json.Unmarshal(req.Params, &params); err == nil {
				plugindebug.ConfigureFromInitializeConfig(params.Config)
				var health string
				if err := s.call(req.Method, func() { health = handler.Initialize(params) }); err != nil {
					health = HealthError
				}
				if health == "" {
					health = HealthOK
				}
				s.initResp.Health = health
			} else {
				Log("initialize params decode failed: %v", err)
			}
			if req.hasID() {
				sendResult(req.ID, s.initResp)
			}

		case "getStatus", "refresh":
			s.handleStatus(req)

		case "performAction":
			s.handleAction(req)

		case "shutdown":
			s.handleShutdown(req)
			return

		default:
			if req.hasID() {
				sendError(req.ID, -32601, fmt.Sprintf("unknown method: %s", req.Method), false, "")
			}
		}
	}

	// stdin closed: give in-flight requests a bounded window to finish
	// writing their responses, without hanging forever on a stuck handler.
	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		Log("stdin closed with handler calls still running; exiting")
	}
}

// parsePerformActionParams accepts the host/companion payload shapes used in
// the wild: payload map[string]string, payload map with non-string values,
// and top-level query/value fields outside payload.
func parsePerformActionParams(raw json.RawMessage) (string, map[string]string) {
	payload := map[string]string{}
	if len(raw) == 0 {
		return "", payload
	}

	var loose struct {
		ActionID string          `json:"actionId"`
		Payload  json.RawMessage `json:"payload"`
		Query    any             `json:"query"`
		Value    any             `json:"value"`
		PageID   any             `json:"pageId"`
		URL      any             `json:"url"`
		Q        any             `json:"q"`
		Text     any             `json:"text"`
		Search   any             `json:"search"`
	}
	if err := json.Unmarshal(raw, &loose); err != nil {
		return "", payload
	}

	if len(loose.Payload) > 0 && string(loose.Payload) != "null" {
		var asStrings map[string]string
		if err := json.Unmarshal(loose.Payload, &asStrings); err == nil {
			for k, v := range asStrings {
				payload[k] = v
			}
		} else {
			var asAny map[string]any
			if err := json.Unmarshal(loose.Payload, &asAny); err == nil {
				for k, v := range asAny {
					if s := anyToString(v); s != "" {
						payload[k] = s
					}
				}
			}
		}
	}

	// Promote top-level fields when payload omitted them.
	setIfEmpty := func(key string, v any) {
		if strings.TrimSpace(payload[key]) != "" {
			return
		}
		if s := anyToString(v); s != "" {
			payload[key] = s
		}
	}
	setIfEmpty("query", loose.Query)
	setIfEmpty("value", loose.Value)
	setIfEmpty("pageId", loose.PageID)
	setIfEmpty("url", loose.URL)
	setIfEmpty("q", loose.Q)
	setIfEmpty("text", loose.Text)
	setIfEmpty("search", loose.Search)

	return strings.TrimSpace(loose.ActionID), payload
}

func anyToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		// JSON numbers
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%t", t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			if s := anyToString(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return strings.TrimSpace(fmt.Sprintf("%v", t))
		}
		s := strings.TrimSpace(string(b))
		return strings.Trim(s, `"`)
	}
}
