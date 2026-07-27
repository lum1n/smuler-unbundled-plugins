package sdk

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
// Call this from your Handler.GetStatus() implementation.
func Emit(event Event) {
	if currentPluginID != "" {
		event.PluginID = currentPluginID
	}
	event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	params := eventParams{Event: event}
	payload, err := json.Marshal(params)
	if err != nil {
		Log("json marshal error in Emit: %v", err)
		return
	}
	fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","method":"event","params":%s}`+"\n", string(payload))
}

// --- internal ---

var currentPluginID string

type eventParams struct {
	Event Event `json:"event"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
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

func sendResult(id int, result interface{}) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
	data, err := json.Marshal(resp)
	if err != nil {
		Log("json marshal error: %v", err)
		return
	}
	fmt.Fprintf(os.Stdout, "%s\n", string(data))
}

func sendError(id int, code int, message string, retryable bool, suggestedAction string) {
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
	fmt.Fprintf(os.Stdout, "%s\n", string(data))
}

// Run starts the JSON-RPC stdio loop. It blocks until the plugin receives
// "shutdown" or stdin closes. pluginID and pluginVersion are sent to the
// host during the initialize handshake.
func Run(pluginID, pluginVersion string, handler Handler) {
	currentPluginID = pluginID

	defer func() {
		if r := recover(); r != nil {
			Log("panic: %v", r)
		}
	}()

	initResp := initPayload{
		Type:            "initialized",
		ProtocolVersion: "0.1.0",
		PluginVersion:   pluginVersion,
		Health:          HealthOK,
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(nil, 2*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			var params InitializeParams
			if err := json.Unmarshal(req.Params, &params); err == nil {
				plugindebug.ConfigureFromInitializeConfig(params.Config)
				initResp.Health = handler.Initialize(params)
				if initResp.Health == "" {
					initResp.Health = HealthOK
				}
			}
			sendResult(req.ID, initResp)

		case "getStatus", "refresh":
			sendResult(req.ID, handler.GetStatus())

		case "performAction":
			actionID, payload := parsePerformActionParams(req.Params)
			if actionID == "" {
				sendError(req.ID, -32602, "performAction missing actionId", false, "Retry the action.")
				continue
			}
			if richer, ok := handler.(ActionResultHandler); ok {
				sendResult(req.ID, richer.PerformActionResult(actionID, payload))
			} else {
				success, data := handler.PerformAction(actionID, payload)
				result := ActionResult{Success: success, Data: data}
				if !success {
					result.Error = data
				}
				sendResult(req.ID, result)
			}
		case "shutdown":
			handler.Shutdown()
			sendResult(req.ID, nil)
			return

		default:
			sendError(req.ID, -32601, fmt.Sprintf("unknown method: %s", req.Method), false, "")
		}
	}

	if err := scanner.Err(); err != nil {
		Log("stdin scanner error: %v", err)
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
