package proxy

import (
	"context"
	"encoding/json"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponsesParseStringInput(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse string input: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected user role, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "hello world" {
		t.Fatalf("expected hello world, got %v", msgs[0].Content)
	}
}

func TestResponsesParseArrayInput(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"input_text","text":"loose part"},
		{"type":"function_call_output","call_id":"call_1","output":"42"}
	]`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse array input: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (msgs=%+v)", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected first message user, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "first" {
		t.Fatalf("expected first text, got %v", msgs[0].Content)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" {
		t.Fatalf("expected tool result with call_id call_1, got %+v", msgs[2])
	}
	if got, _ := msgs[2].Content.(string); got != "42" {
		t.Fatalf("expected tool output 42, got %v", msgs[2].Content)
	}
}

func TestResponsesParseCustomToolRoundTrip(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]},
		{"type":"custom_tool_call","call_id":"call_exec","name":"exec","input":"const r = await tools.exec_command({cmd:\"pwd\"}); text(r.output);"},
		{"type":"custom_tool_call_output","call_id":"call_exec","output":[
			{"type":"input_text","text":"Script completed\nOutput:\n"},
			{"type":"input_text","text":"/workspace\n"}
		]}
	]`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse custom tool turn: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected user, assistant tool call, and tool output; got %d (%+v)", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("expected assistant custom tool call, got %+v", msgs[1])
	}
	call := msgs[1].ToolCalls[0]
	if call.ID != "call_exec" || call.Function.Name != "exec" {
		t.Fatalf("unexpected custom call: %+v", call)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("decode wrapped custom input: %v", err)
	}
	if !strings.Contains(args[customToolInputField], `cmd:"pwd"`) {
		t.Fatalf("custom input was not preserved: %q", args[customToolInputField])
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_exec" {
		t.Fatalf("expected matching custom tool output, got %+v", msgs[2])
	}
	if output, _ := msgs[2].Content.(string); output != "Script completed\nOutput:\n/workspace\n" {
		t.Fatalf("unexpected flattened custom tool output: %q", output)
	}
}

func TestResponsesExtractAdditionalTools(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"custom","name":"exec","description":"Run terminal JavaScript"},
			{"type":"function","name":"wait","description":"Wait for execution","parameters":{"type":"object"}}
		]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]}
	]`)

	tools, err := extractResponsesInputTools(raw)
	if err != nil {
		t.Fatalf("extract additional tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected two additional tools, got %d (%+v)", len(tools), tools)
	}
	if tools[0].Type != "custom" || tools[0].Function.Name != "exec" {
		t.Fatalf("unexpected custom tool: %+v", tools[0])
	}
	if tools[1].Type != "function" || tools[1].Function.Name != "wait" {
		t.Fatalf("unexpected function tool: %+v", tools[1])
	}

	merged := mergeResponsesTools(tools[:1], tools)
	if len(merged) != 2 {
		t.Fatalf("expected duplicate exec tool to be removed, got %d", len(merged))
	}
}

func TestResponsesStoreAndLoad(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	resp := &ResponsesObject{
		ID:        "resp_unit_test_001",
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     "claude-sonnet-4.5",
		Output: []ResponseOutputItem{{
			ID:   "msg_x",
			Type: "message",
			Role: "assistant",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "stored hello",
			}},
		}},
		StoredInput: json.RawMessage(`"hi"`),
	}

	if err := saveResponse(resp); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ID != resp.ID || loaded.Model != resp.Model {
		t.Fatalf("loaded mismatch: %+v", loaded)
	}
	if len(loaded.Output) != 1 || loaded.Output[0].Content[0].Text != "stored hello" {
		t.Fatalf("loaded output mismatch: %+v", loaded.Output)
	}
	if string(loaded.StoredInput) != `"hi"` {
		t.Fatalf("stored input mismatch: %s", string(loaded.StoredInput))
	}

	if _, err := loadResponse("does_not_exist"); err == nil {
		t.Fatalf("expected load error for missing id")
	}
}

func TestResponsesPreviousResponseIDExpands(t *testing.T) {
	prev := &ResponsesObject{
		ID:          "resp_prev",
		StoredInput: json.RawMessage(`"earlier user"`),
		Output: []ResponseOutputItem{
			{
				Type: "message",
				Role: "assistant",
				Content: []ResponseContentPart{{
					Type: "output_text",
					Text: "earlier assistant reply",
				}},
			},
			{
				Type:      "function_call",
				CallID:    "call_prev",
				Name:      "lookup",
				Arguments: `{"q":"x"}`,
			},
		},
	}

	expanded := expandPreviousResponseHistory(prev)
	if len(expanded) != 3 {
		t.Fatalf("expected 3 messages from history, got %d (%+v)", len(expanded), expanded)
	}
	if expanded[0].Role != "user" {
		t.Fatalf("expected first message to be user, got %+v", expanded[0])
	}
	if expanded[1].Role != "assistant" {
		t.Fatalf("expected second message to be assistant, got %+v", expanded[1])
	}
	if expanded[2].Role != "assistant" || len(expanded[2].ToolCalls) != 1 {
		t.Fatalf("expected third to be assistant with tool_calls, got %+v", expanded[2])
	}
	if expanded[2].ToolCalls[0].ID != "call_prev" {
		t.Fatalf("expected tool call id call_prev, got %+v", expanded[2].ToolCalls[0])
	}
}

func TestResponsesPreviousResponseIDExpandsCustomToolCall(t *testing.T) {
	prev := &ResponsesObject{
		ID:          "resp_custom_prev",
		StoredInput: json.RawMessage(`"run pwd"`),
		Output: []ResponseOutputItem{{
			ID:     "ctc_prev",
			Type:   "custom_tool_call",
			CallID: "call_prev_custom",
			Name:   "exec",
			Input:  "pwd",
		}},
	}

	expanded := expandPreviousResponseHistory(prev)
	if len(expanded) != 2 {
		t.Fatalf("expected user and custom tool call, got %d (%+v)", len(expanded), expanded)
	}
	if expanded[1].Role != "assistant" || len(expanded[1].ToolCalls) != 1 {
		t.Fatalf("expected restored custom tool call, got %+v", expanded[1])
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(expanded[1].ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("decode restored custom input: %v", err)
	}
	if args[customToolInputField] != "pwd" {
		t.Fatalf("expected restored custom input pwd, got %#v", args)
	}
}

// A → B → C: when expanding history starting from C, all of A's and B's
// inputs/outputs must appear before C's. Previously only C's direct parent
// (B) was emitted, dropping A entirely.
func TestResponsesPreviousResponseIDExpandsFullChain(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	a := &ResponsesObject{
		ID:           "resp_a",
		Object:       "response",
		Status:       "completed",
		Model:        "claude-sonnet-4.5",
		StoredInput:  json.RawMessage(`"turn A user"`),
		StoredAt:     time.Now().Unix(),
		Instructions: "be terse",
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn A assistant"}},
		}},
	}
	b := &ResponsesObject{
		ID:                 "resp_b",
		Object:             "response",
		Status:             "completed",
		Model:              "claude-sonnet-4.5",
		StoredInput:        json.RawMessage(`"turn B user"`),
		StoredAt:           time.Now().Unix(),
		PreviousResponseID: a.ID,
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn B assistant"}},
		}},
	}
	if err := saveResponse(a); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := saveResponse(b); err != nil {
		t.Fatalf("save b: %v", err)
	}

	expanded := expandPreviousResponseHistory(b)

	var transcript []string
	for _, m := range expanded {
		role := m.Role
		text, _ := m.Content.(string)
		transcript = append(transcript, role+":"+text)
	}
	got := strings.Join(transcript, "|")
	want := "system:be terse|user:turn A user|assistant:turn A assistant|user:turn B user|assistant:turn B assistant"
	if got != want {
		t.Fatalf("chain order mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// New instructions sent on a continuation request must take effect, even when
// previous_response_id is set. The bug: the old code only attached
// req.Instructions when previous_response_id was empty, silently dropping
// updated system prompts on follow-up turns.
func TestResponsesContinuationKeepsNewInstructions(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	prev := &ResponsesObject{
		ID:          "resp_for_continuation",
		Object:      "response",
		Status:      "completed",
		Model:       "claude-sonnet-4.5",
		StoredInput: json.RawMessage(`"first user message"`),
		StoredAt:    time.Now().Unix(),
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "first reply"}},
		}},
	}
	if err := saveResponse(prev); err != nil {
		t.Fatalf("save prev: %v", err)
	}

	var capturedSystem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedSystem = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second reply",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"second user turn",
		"previous_response_id":"resp_for_continuation",
		"instructions":"speak only French",
		"store":false
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(capturedSystem, "speak only French") {
		t.Fatalf("expected new instructions to reach upstream, payload=%s", capturedSystem)
	}
}

func setupResponsesTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "test-account",
		Enabled:     true,
		AccessToken: "token-test",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}
	cleanup := func() {}
	return h, cleanup
}

func swapKiroEndpointsForTest(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	return func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	}
}

func TestResponsesNonStreamRoundTrip(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "responses non-stream OK",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hi from test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "response" {
		t.Fatalf("expected object=response, got %q", resp.Object)
	}
	if resp.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", resp.Status)
	}
	if len(resp.Output) == 0 {
		t.Fatalf("expected output items, got none")
	}
	if resp.Output[0].Type != "message" || len(resp.Output[0].Content) == 0 {
		t.Fatalf("expected message with content, got %+v", resp.Output[0])
	}
	if resp.Output[0].Content[0].Text != "responses non-stream OK" {
		t.Fatalf("unexpected text: %q", resp.Output[0].Content[0].Text)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("loadResponse: %v", err)
	}
	if loaded.ID != resp.ID {
		t.Fatalf("stored response id mismatch")
	}
}

func TestResponsesNonStreamCustomToolCall(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var upstreamPayload string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamPayload = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_exec",
			"name":      "exec",
			"input":     `{"input":"const r = await tools.exec_command({cmd:\"pwd\"}); text(r.output);"}`,
			"stop":      true,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"run pwd",
		"store":false,
		"tools":[{
			"type":"custom",
			"name":"exec",
			"description":"Run JavaScript that orchestrates terminal tools",
			"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}
		}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamPayload, `"name":"exec"`) ||
		!strings.Contains(upstreamPayload, `"required":["input"]`) {
		t.Fatalf("custom tool was not bridged to Kiro schema: %s", upstreamPayload)
	}

	var resp ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Output) != 1 {
		t.Fatalf("expected one custom tool call, got %+v", resp.Output)
	}
	call := resp.Output[0]
	if call.Type != "custom_tool_call" || call.CallID != "call_exec" || call.Name != "exec" {
		t.Fatalf("unexpected custom tool response item: %+v", call)
	}
	if !strings.Contains(call.Input, `cmd:"pwd"`) {
		t.Fatalf("expected raw custom input, got %q", call.Input)
	}
	if call.Arguments != "" {
		t.Fatalf("custom call must not expose function arguments: %+v", call)
	}
}

func TestResponsesCodexAdditionalToolsRoundTrip(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var upstreamPayload string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamPayload = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_exec_codex",
			"name":      "exec",
			"input":     `{"input":"const r = await tools.exec_command({cmd:\"pwd\"}); text(r.output);"}`,
			"stop":      true,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"store":false,
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{
				"type":"custom",
				"name":"exec",
				"description":"Run JavaScript that orchestrates terminal tools",
				"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}
			}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run pwd"}]}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(upstreamPayload, `"name":"exec"`) ||
		!strings.Contains(upstreamPayload, `"required":["input"]`) {
		t.Fatalf("Codex additional tool was not bridged to Kiro schema: %s", upstreamPayload)
	}

	var resp ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "custom_tool_call" ||
		resp.Output[0].Name != "exec" {
		t.Fatalf("unexpected Codex custom tool response: %+v", resp.Output)
	}
}

func TestResponsesStreamSSE(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "stream chunk",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"stream please","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	bodyBytes, _ := io.ReadAll(rec.Body)
	bodyStr := string(bodyBytes)

	for _, evt := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(bodyStr, evt) {
			t.Fatalf("missing event %q in stream body:\n%s", evt, bodyStr)
		}
	}
	if !strings.Contains(bodyStr, "stream chunk") {
		t.Fatalf("expected stream content delta, got:\n%s", bodyStr)
	}
}

func TestResponsesStreamCustomToolCall(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "call_exec_stream",
			"name":      "exec",
			"input":     `{"input":"pwd"}`,
			"stop":      true,
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"run pwd",
		"stream":true,
		"store":false,
		"tools":[{"type":"custom","name":"exec","description":"Run a terminal command"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	for _, expected := range []string{
		"event: response.output_item.added",
		"event: response.custom_tool_call_input.delta",
		"event: response.custom_tool_call_input.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"type":"custom_tool_call"`,
		`"call_id":"call_exec_stream"`,
		`"input":"pwd"`,
	} {
		if !strings.Contains(bodyStr, expected) {
			t.Fatalf("missing %q in custom tool stream:\n%s", expected, bodyStr)
		}
	}
	if strings.Contains(bodyStr, "response.function_call_arguments.delta") {
		t.Fatalf("custom tool stream emitted function-call argument events:\n%s", bodyStr)
	}
}
