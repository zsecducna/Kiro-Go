package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// --- stop_reason mapping ---------------------------------------------------

func TestAnthropicStopReasonToOpenAIFinish(t *testing.T) {
	cases := map[string]string{
		"end_turn":         "stop",
		"stop_sequence":    "stop",
		"max_tokens":       "length",
		"tool_use":         "tool_calls",
		"content_filtered": "content_filter",
		"":                 "",
		"weird_new_reason": "weird_new_reason",
	}
	for in, want := range cases {
		if got := anthropicStopReasonToOpenAIFinish(in); got != want {
			t.Errorf("stop_reason %q -> %q, want %q", in, got, want)
		}
	}
}

// --- request conversion: OpenAI -> Anthropic -------------------------------

func TestOpenAIToAnthropicMessages(t *testing.T) {
	body := []byte(`{
		"model": "claude-smoke",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "weather in Paris?"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "toolu_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}
			]},
			{"role": "tool", "tool_call_id": "toolu_1", "content": "18C sunny"},
			{"role": "user", "content": "thanks"}
		],
		"tools": [
			{"type": "function", "function": {"name": "get_weather", "description": "w", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}}}}
		],
		"temperature": 0.5
	}`)

	out, err := openAIToAnthropicMessages(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("result not JSON: %v", err)
	}

	if m["system"] != "be terse" {
		t.Errorf("system = %v, want 'be terse'", m["system"])
	}
	// model must NOT be present (buildBedrockBody/URL carry it).
	if _, ok := m["model"]; ok {
		t.Errorf("model should be absent from converted body")
	}
	// max_tokens defaulted (request omitted it).
	if m["max_tokens"] != float64(bedrockOpenAIDefaultMaxTokens) {
		t.Errorf("max_tokens = %v, want default %d", m["max_tokens"], bedrockOpenAIDefaultMaxTokens)
	}
	if m["temperature"] != 0.5 {
		t.Errorf("temperature = %v, want 0.5", m["temperature"])
	}

	msgs, ok := m["messages"].([]interface{})
	if !ok || len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (user, assistant, tool-result-user, user)", len(msgs))
	}

	// messages[1] assistant carries a tool_use block.
	asst := msgs[1].(map[string]interface{})
	if asst["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", asst["role"])
	}
	ablocks := asst["content"].([]interface{})
	tu := ablocks[0].(map[string]interface{})
	if tu["type"] != "tool_use" || tu["id"] != "toolu_1" || tu["name"] != "get_weather" {
		t.Errorf("assistant tool_use block wrong: %v", tu)
	}
	input := tu["input"].(map[string]interface{})
	if input["city"] != "Paris" {
		t.Errorf("tool_use input.city = %v, want Paris", input["city"])
	}

	// messages[2] is a user turn holding the tool_result.
	toolTurn := msgs[2].(map[string]interface{})
	if toolTurn["role"] != "user" {
		t.Fatalf("messages[2].role = %v, want user", toolTurn["role"])
	}
	tr := toolTurn["content"].([]interface{})[0].(map[string]interface{})
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_1" || tr["content"] != "18C sunny" {
		t.Errorf("tool_result block wrong: %v", tr)
	}

	// tools mapped to Anthropic input_schema.
	tools := m["tools"].([]interface{})
	tool0 := tools[0].(map[string]interface{})
	if tool0["name"] != "get_weather" {
		t.Errorf("tool name = %v", tool0["name"])
	}
	if _, ok := tool0["input_schema"].(map[string]interface{}); !ok {
		t.Errorf("tool input_schema missing/typed wrong: %v", tool0["input_schema"])
	}
}

func TestOpenAIToAnthropicMessages_MaxTokensAndImages(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("x")) + `"}}
			]}
		],
		"max_tokens": 128
	}`)
	out, err := openAIToAnthropicMessages(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(out, &m)
	if m["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v, want 128", m["max_tokens"])
	}
	blocks := m["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if len(blocks) != 2 {
		t.Fatalf("content blocks = %d, want 2 (text+image)", len(blocks))
	}
	img := blocks[1].(map[string]interface{})
	if img["type"] != "image" {
		t.Errorf("second block type = %v, want image", img["type"])
	}
	src := img["source"].(map[string]interface{})
	if src["media_type"] != "image/png" || src["type"] != "base64" {
		t.Errorf("image source wrong: %v", src)
	}
}

// --- non-stream response conversion: Anthropic -> OpenAI --------------------

func TestAnthropicMessageToOpenAIResponse(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		resp := []byte(`{"content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)
		out, in, o, err := anthropicMessageToOpenAIResponse(resp, "claude-smoke")
		if err != nil {
			t.Fatal(err)
		}
		if in != 10 || o != 3 {
			t.Errorf("tokens = %d/%d, want 10/3", in, o)
		}
		choice := out["choices"].([]map[string]interface{})[0]
		if choice["finish_reason"] != "stop" {
			t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
		}
		msg := choice["message"].(map[string]interface{})
		if msg["content"] != "hi there" {
			t.Errorf("content = %v", msg["content"])
		}
	})

	t.Run("tool_use", func(t *testing.T) {
		resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_9","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":8}}`)
		out, _, _, err := anthropicMessageToOpenAIResponse(resp, "m")
		if err != nil {
			t.Fatal(err)
		}
		choice := out["choices"].([]map[string]interface{})[0]
		if choice["finish_reason"] != "tool_calls" {
			t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
		}
		msg := choice["message"].(map[string]interface{})
		if msg["content"] != nil {
			t.Errorf("content should be nil when tool_calls present, got %v", msg["content"])
		}
		tc := msg["tool_calls"].([]map[string]interface{})[0]
		if tc["id"] != "toolu_9" {
			t.Errorf("tool_call id = %v", tc["id"])
		}
		fn := tc["function"].(map[string]string)
		if fn["name"] != "lookup" || !strings.Contains(fn["arguments"], `"q":"x"`) {
			t.Errorf("tool_call function wrong: %v", fn)
		}
	})
}

func TestOpenAIToAnthropicMessages_ParamsAndSkips(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": ""},
			{"role": "assistant", "content": null},
			{"role": "user", "content": "hi"}
		],
		"temperature": 1.7,
		"stop": ["STOP", ""],
		"tools": [{"type": "function", "function": {"name": "f"}}],
		"tool_choice": "required"
	}`)
	out, err := openAIToAnthropicMessages(body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(out, &m)

	// Empty user + empty assistant turns are skipped; only "hi" remains.
	msgs := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1 (empty turns skipped)", len(msgs))
	}

	// temperature clamped 1.7 -> 1.
	if m["temperature"] != float64(1) {
		t.Errorf("temperature = %v, want clamped 1", m["temperature"])
	}
	// stop -> stop_sequences, empty dropped.
	seqs := m["stop_sequences"].([]interface{})
	if len(seqs) != 1 || seqs[0] != "STOP" {
		t.Errorf("stop_sequences = %v, want [STOP]", seqs)
	}
	// tool_choice required -> {type: any}.
	tc := m["tool_choice"].(map[string]interface{})
	if tc["type"] != "any" {
		t.Errorf("tool_choice = %v, want type any", tc)
	}
}

func TestOpenAIToolChoiceNamed(t *testing.T) {
	raw := json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`)
	got := openAIToolChoiceToAnthropic(raw)
	if got["type"] != "tool" || got["name"] != "get_weather" {
		t.Errorf("named tool_choice = %v, want {tool, get_weather}", got)
	}
}

func TestAnthropicMessageToOpenAIResponse_TextWithToolCalls(t *testing.T) {
	// Claude often emits prose then a tool call; both must survive (parity with
	// the streaming path, which emits text deltas before the tool chunks).
	resp := []byte(`{"content":[{"type":"text","text":"let me check"},{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":6}}`)
	out, _, _, err := anthropicMessageToOpenAIResponse(resp, "m")
	if err != nil {
		t.Fatal(err)
	}
	choice := out["choices"].([]map[string]interface{})[0]
	msg := choice["message"].(map[string]interface{})
	if msg["content"] != "let me check" {
		t.Errorf("content = %v, want 'let me check' preserved alongside tool_calls", msg["content"])
	}
	if _, ok := msg["tool_calls"].([]map[string]interface{}); !ok {
		t.Errorf("tool_calls missing")
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
}

// --- synthetic streaming conversion ----------------------------------------

// wrapAnthropicFrame wraps an inner Anthropic event JSON in the AWS event-stream
// frame + {"bytes": base64(...)} envelope that readBedrockEventStream expects,
// reusing buildFrame from bedrock_eventstream_test.go.
func wrapAnthropicFrame(inner string) []byte {
	env, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(inner))})
	return buildFrame(map[string]string{
		":message-type": "event", ":event-type": "chunk", ":content-type": "application/json",
	}, env)
}

func TestBedrockOpenAIStreamSynthetic(t *testing.T) {
	events := []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":42}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	}
	var stream bytes.Buffer
	for _, e := range events {
		stream.Write(wrapAnthropicFrame(e))
	}

	conv := newBedrockOpenAIStreamConv("claude-smoke")
	var out bytes.Buffer
	err := readBedrockEventStream(bytes.NewReader(stream.Bytes()), func(_ string, aj []byte) error {
		return conv.onEvent(&out, nil, aj)
	})
	if err != nil {
		t.Fatalf("readBedrockEventStream: %v", err)
	}
	conv.finish(&out, nil)

	if conv.inputTokens != 42 || conv.outputTokens != 7 {
		t.Errorf("tokens = %d/%d, want 42/7", conv.inputTokens, conv.outputTokens)
	}

	// Parse the emitted SSE chunks.
	var text strings.Builder
	var toolID, toolName string
	var toolArgs strings.Builder
	var finalFinish string
	var finalUsageTotal float64
	sawDone := false

	for _, block := range strings.Split(out.String(), "\n\n") {
		line := strings.TrimSpace(block)
		if line == "" {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("chunk not JSON: %s", data)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Errorf("object = %v, want chat.completion.chunk", chunk["object"])
		}
		choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
		delta, _ := choice["delta"].(map[string]interface{})
		if c, ok := delta["content"].(string); ok {
			text.WriteString(c)
		}
		if tcs, ok := delta["tool_calls"].([]interface{}); ok {
			tc := tcs[0].(map[string]interface{})
			if tc["index"] != float64(0) {
				t.Errorf("tool_call index = %v, want 0", tc["index"])
			}
			if id, ok := tc["id"].(string); ok && id != "" {
				toolID = id
			}
			if fn, ok := tc["function"].(map[string]interface{}); ok {
				if n, ok := fn["name"].(string); ok && n != "" {
					toolName = n
				}
				if a, ok := fn["arguments"].(string); ok {
					toolArgs.WriteString(a)
				}
			}
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			finalFinish = fr
			if usage, ok := chunk["usage"].(map[string]interface{}); ok {
				finalUsageTotal, _ = usage["total_tokens"].(float64)
			}
		}
	}

	if text.String() != "Hello world" {
		t.Errorf("streamed text = %q, want 'Hello world'", text.String())
	}
	if toolID != "toolu_1" || toolName != "get_weather" {
		t.Errorf("tool id/name = %q/%q, want toolu_1/get_weather", toolID, toolName)
	}
	if toolArgs.String() != `{"city":"Paris"}` {
		t.Errorf("reassembled tool args = %q, want {\"city\":\"Paris\"}", toolArgs.String())
	}
	if finalFinish != "tool_calls" {
		t.Errorf("final finish_reason = %q, want tool_calls", finalFinish)
	}
	if finalUsageTotal != 49 {
		t.Errorf("final usage.total_tokens = %v, want 49", finalUsageTotal)
	}
	if !sawDone {
		t.Errorf("stream did not end with data: [DONE]")
	}
}
