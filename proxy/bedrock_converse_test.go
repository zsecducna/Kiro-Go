package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestLooksLikeBedrockModelID(t *testing.T) {
	yes := []string{
		"anthropic.claude-3-5-sonnet-20241022-v2:0",
		"us.amazon.nova-pro-v1:0",
		"meta.llama3-70b-instruct-v1:0",
		"us.deepseek.r1-v1:0",
	}
	no := []string{"claude-3-5-sonnet", "claude-smoke", "nova"}
	for _, s := range yes {
		if !looksLikeBedrockModelID(s) {
			t.Errorf("%q should look like a Bedrock id", s)
		}
	}
	for _, s := range no {
		if looksLikeBedrockModelID(s) {
			t.Errorf("%q should NOT look like a Bedrock id (alias)", s)
		}
	}
}

func TestAnthropicToolChoiceToConverse(t *testing.T) {
	if tc := anthropicToolChoiceToConverse(json.RawMessage(`{"type":"any"}`)); tc["any"] == nil {
		t.Errorf("any -> %v", tc)
	}
	if tc := anthropicToolChoiceToConverse(json.RawMessage(`{"type":"tool","name":"f"}`)); tc["tool"].(map[string]interface{})["name"] != "f" {
		t.Errorf("named tool -> %v", tc)
	}
	if tc := anthropicToolChoiceToConverse(nil); tc != nil {
		t.Errorf("nil -> %v, want nil", tc)
	}
}

// A Converse reasoning delta must open a THINKING-typed content block (not text),
// then attach thinking_delta — otherwise the emitted Anthropic stream is malformed.
func TestConverseStreamReasoning(t *testing.T) {
	frames := []struct{ typ, payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"thinking..."}}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":3,"outputTokens":2}}`},
	}
	var stream bytes.Buffer
	for _, f := range frames {
		stream.Write(converseFrame(f.typ, f.payload))
	}
	conv := newConverseStreamConv()
	var events []map[string]interface{}
	emit := func(aj []byte) error {
		var e map[string]interface{}
		_ = json.Unmarshal(aj, &e)
		events = append(events, e)
		return nil
	}
	_ = readBedrockConverseEventStream(bytes.NewReader(stream.Bytes()), func(et string, pl []byte) error {
		return conv.process(et, pl, emit)
	})
	_ = conv.finalize(emit)

	var sawThinkingStart, sawThinkingDelta bool
	for _, e := range events {
		if e["type"] == "content_block_start" {
			if cb, ok := e["content_block"].(map[string]interface{}); ok && cb["type"] == "thinking" {
				sawThinkingStart = true
			}
		}
		if e["type"] == "content_block_delta" {
			if d, ok := e["delta"].(map[string]interface{}); ok && d["type"] == "thinking_delta" {
				sawThinkingDelta = true
			}
		}
	}
	if !sawThinkingStart {
		t.Errorf("reasoning stream did not open a thinking-typed content_block_start")
	}
	if !sawThinkingDelta {
		t.Errorf("reasoning stream did not emit thinking_delta")
	}
}

func TestConverseStopReasonToAnthropic(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "end_turn",
		"tool_use":      "tool_use",
		"max_tokens":    "max_tokens",
		"stop_sequence": "stop_sequence",
		"":              "end_turn",
	}
	for in, want := range cases {
		if got := converseStopReasonToAnthropic(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestAnthropicToConverseBody(t *testing.T) {
	body := []byte(`{
		"system": "be terse",
		"max_tokens": 100,
		"temperature": 0.4,
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "f", "input": {"x": 1}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "42"}]}
		],
		"tools": [{"name": "f", "description": "d", "input_schema": {"type": "object"}}]
	}`)
	out, err := anthropicToConverseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(out, &m)

	sys := m["system"].([]interface{})[0].(map[string]interface{})
	if sys["text"] != "be terse" {
		t.Errorf("system = %v", m["system"])
	}
	inf := m["inferenceConfig"].(map[string]interface{})
	if inf["maxTokens"] != float64(100) || inf["temperature"] != 0.4 {
		t.Errorf("inferenceConfig = %v", inf)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	// assistant tool_use -> toolUse block
	asst := msgs[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	tu := asst["toolUse"].(map[string]interface{})
	if tu["toolUseId"] != "t1" || tu["name"] != "f" {
		t.Errorf("toolUse = %v", tu)
	}
	// tool_result -> toolResult block
	tr := msgs[2].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	trb := tr["toolResult"].(map[string]interface{})
	if trb["toolUseId"] != "t1" {
		t.Errorf("toolResult = %v", trb)
	}
	// tools -> toolConfig.tools[].toolSpec
	toolSpec := m["toolConfig"].(map[string]interface{})["tools"].([]interface{})[0].(map[string]interface{})["toolSpec"].(map[string]interface{})
	if toolSpec["name"] != "f" {
		t.Errorf("toolSpec = %v", toolSpec)
	}
	if _, ok := toolSpec["inputSchema"].(map[string]interface{})["json"]; !ok {
		t.Errorf("toolSpec inputSchema.json missing: %v", toolSpec["inputSchema"])
	}
}

func TestConverseResponseToAnthropicMessage(t *testing.T) {
	resp := []byte(`{
		"output": {"message": {"content": [
			{"text": "let me check"},
			{"toolUse": {"toolUseId": "t9", "name": "lookup", "input": {"q": "x"}}}
		]}},
		"stopReason": "tool_use",
		"usage": {"inputTokens": 12, "outputTokens": 4}
	}`)
	out, in, o, err := converseResponseToAnthropicMessage(resp, "nova")
	if err != nil {
		t.Fatal(err)
	}
	if in != 12 || o != 4 {
		t.Errorf("tokens = %d/%d, want 12/4", in, o)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(out, &m)
	if m["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v", m["stop_reason"])
	}
	content := m["content"].([]interface{})
	if content[0].(map[string]interface{})["type"] != "text" {
		t.Errorf("content[0] = %v", content[0])
	}
	if content[1].(map[string]interface{})["type"] != "tool_use" {
		t.Errorf("content[1] = %v", content[1])
	}
}

// converseFrame wraps a Converse event JSON in an AWS event-stream frame with the
// event name as :event-type (the payload is the raw event JSON — no bytes envelope).
func converseFrame(eventType, payload string) []byte {
	return buildFrame(map[string]string{
		":message-type": "event", ":event-type": eventType, ":content-type": "application/json",
	}, []byte(payload))
}

func TestConverseStreamSynthetic(t *testing.T) {
	frames := []struct{ typ, payload string }{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hi"}}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":" there"}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"t1","name":"getw"}}}`},
		{"contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"a\":1}"}}}`},
		{"contentBlockStop", `{"contentBlockIndex":1}`},
		{"messageStop", `{"stopReason":"tool_use"}`},
		{"metadata", `{"usage":{"inputTokens":11,"outputTokens":5}}`},
	}
	var stream bytes.Buffer
	for _, f := range frames {
		stream.Write(converseFrame(f.typ, f.payload))
	}

	conv := newConverseStreamConv()
	var events []map[string]interface{}
	emit := func(anthropicJSON []byte) error {
		var e map[string]interface{}
		if err := json.Unmarshal(anthropicJSON, &e); err != nil {
			t.Fatalf("emitted non-JSON: %s", anthropicJSON)
		}
		events = append(events, e)
		return nil
	}
	err := readBedrockConverseEventStream(bytes.NewReader(stream.Bytes()), func(et string, pl []byte) error {
		return conv.process(et, pl, emit)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.finalize(emit); err != nil {
		t.Fatal(err)
	}

	if conv.inputTokens != 11 || conv.outputTokens != 5 {
		t.Errorf("tokens = %d/%d, want 11/5", conv.inputTokens, conv.outputTokens)
	}

	types := make([]string, len(events))
	var textOut string
	var toolArgs string
	var finalStop string
	for i, e := range events {
		types[i], _ = e["type"].(string)
		if e["type"] == "content_block_delta" {
			d := e["delta"].(map[string]interface{})
			switch d["type"] {
			case "text_delta":
				textOut += d["text"].(string)
			case "input_json_delta":
				toolArgs += d["partial_json"].(string)
			}
		}
		if e["type"] == "message_delta" {
			finalStop = e["delta"].(map[string]interface{})["stop_reason"].(string)
		}
	}

	if textOut != "Hi there" {
		t.Errorf("text = %q, want 'Hi there'", textOut)
	}
	if toolArgs != `{"a":1}` {
		t.Errorf("tool args = %q, want {\"a\":1}", toolArgs)
	}
	if finalStop != "tool_use" {
		t.Errorf("final stop_reason = %q, want tool_use", finalStop)
	}
	// Ordering: first event message_start, a lazy content_block_start(text) precedes
	// the first text delta, and the last two events are message_delta, message_stop.
	if types[0] != "message_start" {
		t.Errorf("first event = %q, want message_start", types[0])
	}
	if types[1] != "content_block_start" {
		t.Errorf("second event = %q, want lazy content_block_start", types[1])
	}
	if n := len(types); types[n-1] != "message_stop" || types[n-2] != "message_delta" {
		t.Errorf("tail events = %v, want [..., message_delta, message_stop]", types[n-2:])
	}
	// A tool_use content_block_start must appear for index 1.
	sawToolStart := false
	for _, e := range events {
		if e["type"] == "content_block_start" {
			if cb, ok := e["content_block"].(map[string]interface{}); ok && cb["type"] == "tool_use" && cb["id"] == "t1" {
				sawToolStart = true
			}
		}
	}
	if !sawToolStart {
		t.Errorf("missing tool_use content_block_start")
	}
}
