package sdk

import (
	"encoding/json"
	"testing"
)

type testHandler struct {
	initCalled   bool
	initParams   InitializeParams
	statusCalled bool
	shutdownDone bool
}

func (h *testHandler) Initialize(params InitializeParams) string {
	h.initCalled = true
	h.initParams = params
	return HealthOK
}

func (h *testHandler) GetStatus() Snapshot {
	h.statusCalled = true
	return Snapshot{
		PluginID: "test",
		State:    StateReady,
		Summary: Summary{
			Title:    "Test",
			Value:    "0",
			Trend:    TrendSteady,
			Severity: SeverityInfo,
			IconHint: "",
		},
		Items:        []Item{},
		Actions:      []Action{},
		Alerts:       []Alert{},
		RefreshAfter: 30,
		Health:       HealthOK,
	}
}

func (h *testHandler) Shutdown() {
	h.shutdownDone = true
}

func TestSnapshotRoundtrip(t *testing.T) {
	snap := Snapshot{
		PluginID: "github",
		State:    StateReady,
		Summary: Summary{
			Title:    "Reviews",
			Value:    "3",
			Trend:    TrendUp,
			Severity: SeverityWarning,
			IconHint: "",
		},
		Items: []Item{
			{
				ID:        "1",
				Title:     "Fix auth bug",
				Subtitle:  "owner/repo",
				Detail:    "Opened 2h ago",
				Severity:  SeverityWarning,
				Timestamp: "2024-01-01T00:00:00Z",
				DeepLink:  "https://github.com/owner/repo/pull/1",
				Actions:   []Action{{ID: "open", Label: "Open in Browser"}},
			},
		},
		Actions: []Action{},
		Alerts:  []Alert{{ID: "rate", Severity: SeverityWarning, Message: "Rate limit approaching"}},
		RefreshAfter: 60,
		Health:       HealthOK,
	}

	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	if decoded.PluginID != "github" {
		t.Errorf("pluginId: got %q, want %q", decoded.PluginID, "github")
	}
	if decoded.State != StateReady {
		t.Errorf("state: got %q, want %q", decoded.State, StateReady)
	}
	if decoded.Health != HealthOK {
		t.Errorf("health: got %q, want %q", decoded.Health, HealthOK)
	}
	if len(decoded.Items) != 1 {
		t.Fatalf("items: got %d, want 1", len(decoded.Items))
	}
	if decoded.Items[0].Title != "Fix auth bug" {
		t.Errorf("item title: got %q, want %q", decoded.Items[0].Title, "Fix auth bug")
	}
	if decoded.Items[0].DeepLink != "https://github.com/owner/repo/pull/1" {
		t.Errorf("deepLink: got %q", decoded.Items[0].DeepLink)
	}
}

func TestInitializeParamsRoundtrip(t *testing.T) {
	params := InitializeParams{
		ProtocolVersion: "0.1.0",
		PluginID:        "test-plugin",
		Config:          map[string]string{"repo": "/tmp/test"},
		Auth:            &AuthContext{AccountID: "token123"},
		ProviderAuths: []ProviderAuthContext{
			{
				ProviderID:  "claude",
				Kind:        "oauth",
				AccessToken: "at-secret",
				DisplayName: "claude@example.com",
			},
		},
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	var decoded InitializeParams
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	if decoded.ProtocolVersion != "0.1.0" {
		t.Errorf("protocolVersion: got %q", decoded.ProtocolVersion)
	}
	if decoded.Auth == nil || decoded.Auth.AccountID != "token123" {
		t.Errorf("auth: got %v", decoded.Auth)
	}
	if len(decoded.ProviderAuths) != 1 {
		t.Fatalf("providerAuths: got %d, want 1", len(decoded.ProviderAuths))
	}
	if decoded.ProviderAuths[0].AccessToken != "at-secret" {
		t.Errorf("accessToken: got %q", decoded.ProviderAuths[0].AccessToken)
	}
}

func TestEventRoundtrip(t *testing.T) {
	ev := Event{
		Type:      "pr.review_requested",
		PluginID:  "github",
		Message:   "New review from alice",
		Severity:  SeverityWarning,
		Data:      map[string]string{"url": "https://github.com/owner/repo/pull/1", "repo": "owner/repo"},
		Timestamp: "2024-01-01T00:00:00Z",
	}

	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	var decoded Event
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}

	if decoded.Type != "pr.review_requested" {
		t.Errorf("type: got %q", decoded.Type)
	}
	if decoded.Data["url"] != "https://github.com/owner/repo/pull/1" {
		t.Errorf("data.url: got %q", decoded.Data["url"])
	}
}

func TestRPCRequestParsing(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"getStatus","params":{}}`

	var req rpcRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	if req.ID != 1 {
		t.Errorf("id: got %d, want 1", req.ID)
	}
	if req.Method != "getStatus" {
		t.Errorf("method: got %q, want getStatus", req.Method)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc: got %q, want 2.0", req.JSONRPC)
	}
}

func TestActionResultWithWindow(t *testing.T) {
	result := ActionWindow(WindowContent{
		ID:       "demo.window",
		Title:    "Demo",
		Subtitle: "Plugin",
		IconHint: "doc.text",
		Sections: []WindowSection{
			{
				ID:    "main",
				Title: "Notes",
				Blocks: []WindowBlock{
					{ID: "1", Text: "First", Style: WindowBlockBullet},
					{ID: "2", Text: "code sample", Style: WindowBlockCode},
				},
			},
		},
	})

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ActionResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Success {
		t.Fatal("expected success")
	}
	if decoded.Window == nil {
		t.Fatal("expected window")
	}
	if decoded.Window.ID != "demo.window" {
		t.Errorf("window.id: got %q", decoded.Window.ID)
	}
	if len(decoded.Window.Sections) != 1 || len(decoded.Window.Sections[0].Blocks) != 2 {
		t.Fatalf("unexpected sections/blocks: %+v", decoded.Window.Sections)
	}
	if decoded.Window.Sections[0].Blocks[0].Style != WindowBlockBullet {
		t.Errorf("block style: got %q", decoded.Window.Sections[0].Blocks[0].Style)
	}
}

func TestActionResultWithAIWindow(t *testing.T) {
	window := WindowContent{
		ID:       "demo.summarize",
		Title:    "Demo",
		IconHint: "doc.text",
		Sections: []WindowSection{{
			ID:    "loading",
			Title: "Summary",
			Blocks: []WindowBlock{{ID: "generating", Text: "Generating summary...", Style: WindowBlockParagraph}},
		}},
	}
	result := ActionAIWindow(window, AIAssist{
		Task:         AITaskSummarizeBullets,
		Input:        "diff --git a/x",
		WindowID:     window.ID,
		SectionTitle: "Summary",
	})

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ActionResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.AI == nil || decoded.AI.Task != AITaskSummarizeBullets {
		t.Fatalf("ai: %+v", decoded.AI)
	}
	if decoded.AI.WindowID != "demo.summarize" {
		t.Errorf("ai.windowId: got %q", decoded.AI.WindowID)
	}
}

func TestActionAITaskHelpers(t *testing.T) {
	tasks := []string{
		AITaskSummarizeBullets,
		AITaskExplain,
		AITaskRiskReview,
		AITaskTriageNext,
		AITaskDraftReply,
		AITaskExtractActions,
	}
	for _, task := range tasks {
		result := ActionAITask(AIWindowOpts{
			ID:    "demo." + task,
			Title: "Demo",
			Task:  task,
			Input: "sample input",
		})
		if result.Window == nil || result.AI == nil {
			t.Fatalf("task %s: missing window/ai", task)
		}
		if result.AI.Task != task {
			t.Errorf("task %s: ai.task=%q", task, result.AI.Task)
		}
		if result.Window.Sections[0].ID != "loading" {
			t.Errorf("task %s: expected loading section", task)
		}
		if AITaskSectionTitle(task) == "" || AITaskLoadingLabel(task) == "" {
			t.Errorf("task %s: empty labels", task)
		}
	}
}

func TestRPCResponseMarshaling(t *testing.T) {
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      1,
		Result: Snapshot{
			PluginID: "test",
			State:    StateReady,
			Summary: Summary{
				Title: "OK", Value: "0", Trend: TrendSteady,
				Severity: SeverityInfo, IconHint: "",
			},
			Items:        []Item{},
			Actions:      []Action{},
			Alerts:       []Alert{},
			RefreshAfter: 30,
			Health:       HealthOK,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc: got %v", decoded["jsonrpc"])
	}
	if result, ok := decoded["result"].(map[string]interface{}); !ok {
		t.Errorf("result missing or not an object")
	} else if result["health"] != "ok" {
		t.Errorf("result.health: got %v", result["health"])
	}
}

func TestParsePerformActionParams(t *testing.T) {
	actionID, payload := parsePerformActionParams([]byte(`{
		"actionId": "searchDocs",
		"payload": {"query": "onboarding"}
	}`))
	if actionID != "searchDocs" || payload["query"] != "onboarding" {
		t.Fatalf("string payload: action=%q payload=%v", actionID, payload)
	}

	actionID, payload = parsePerformActionParams([]byte(`{
		"actionId": "searchDocs",
		"payload": {"query": 42}
	}`))
	if actionID != "searchDocs" || payload["query"] != "42" {
		t.Fatalf("numeric payload: action=%q payload=%v", actionID, payload)
	}

	actionID, payload = parsePerformActionParams([]byte(`{
		"actionId": "searchDocs",
		"query": "vacation policy"
	}`))
	if actionID != "searchDocs" || payload["query"] != "vacation policy" {
		t.Fatalf("top-level query: action=%q payload=%v", actionID, payload)
	}
}
