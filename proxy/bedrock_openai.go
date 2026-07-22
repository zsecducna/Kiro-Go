package proxy

// OpenAI-compatible surface for the native Bedrock provider.
//
// Bedrock's Anthropic models accept the native Anthropic Messages wire format,
// not OpenAI Chat Completions. The Anthropic Messages endpoint therefore serves
// Bedrock as a transparent passthrough (see bedrock.go). The OpenAI endpoint
// cannot: it must (1) convert the incoming OpenAI Chat Completions body into an
// Anthropic Messages body, (2) invoke Bedrock exactly like the Anthropic path,
// and (3) convert the Anthropic response / SSE back into OpenAI
// chat.completion(.chunk) objects.
//
// IMPORTANT: our Bedrock side is native Anthropic (readBedrockEventStream yields
// Anthropic events: content_block_delta, message_delta.stop_reason, tool_use).
// Only the OPENAI-TARGET shapes and the stop_reason->finish_reason table are
// mirrored from AWS's aws-samples/bedrock-access-gateway (MIT-0); we do NOT read
// its Converse field names (contentBlockDelta/toolUse/stopReason).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"kiro-go/logger"
)

// bedrockOpenAIDefaultMaxTokens is used when an OpenAI request omits max_tokens;
// Anthropic Messages requires the field, so a sane default keeps requests valid.
const bedrockOpenAIDefaultMaxTokens = 4096

// anthropicStopReasonToOpenAIFinish maps an Anthropic stop_reason to an OpenAI
// finish_reason. Table borrowed verbatim from the AWS sample's finish-reason
// mapping (the only OpenAI-target logic reused). Unknown values pass through
// lowercased so a new Anthropic reason degrades gracefully instead of vanishing.
func anthropicStopReasonToOpenAIFinish(sr string) string {
	switch strings.ToLower(strings.TrimSpace(sr)) {
	case "":
		return ""
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "content_filtered":
		return "content_filter"
	default:
		return strings.ToLower(strings.TrimSpace(sr))
	}
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI Chat Completions -> Anthropic Messages
// ---------------------------------------------------------------------------

// openAIChatRequest is the minimal subset of the OpenAI Chat Completions request
// we need to reconstruct an Anthropic Messages body. Fields we don't translate
// (n, presence_penalty, ...) are intentionally ignored.
type openAIChatRequest struct {
	Messages            []OpenAIMessage `json:"messages"`
	Tools               []openAITool    `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	Stop                json.RawMessage `json:"stop"`
}

// openAITool is one OpenAI function-tool declaration. Its function.parameters is
// a JSON Schema object that maps directly onto Anthropic's tool input_schema.
type openAITool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// openAIToAnthropicMessages converts a raw OpenAI Chat Completions body into an
// Anthropic Messages body (without model/anthropic_version; doBedrockInvoke's
// buildBedrockBody adds those). It handles system/developer messages, text and
// image user content, assistant tool_calls, and tool-result turns. Errors are
// returned pre-stream so the caller can fail over to another account.
func openAIToAnthropicMessages(rawBody []byte) ([]byte, error) {
	var req openAIChatRequest
	if err := json.Unmarshal(rawBody, &req); err != nil {
		return nil, fmt.Errorf("bedrock openai: invalid request body: %w", err)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("bedrock openai: request has no messages")
	}

	var systemParts []string
	// anthropicMessages accumulates the translated turns. tool-result turns are
	// merged into a trailing user message so multiple tool outputs form one turn,
	// which is what Anthropic expects after an assistant tool_use turn.
	anthropicMessages := make([]map[string]interface{}, 0, len(req.Messages))

	// appendToolResult attaches a tool_result block to the current trailing user
	// message when it already holds tool_results, else opens a new user turn.
	appendToolResult := func(block map[string]interface{}) {
		if n := len(anthropicMessages); n > 0 {
			last := anthropicMessages[n-1]
			if last["role"] == "user" {
				if blocks, ok := last["content"].([]map[string]interface{}); ok && len(blocks) > 0 {
					if blocks[0]["type"] == "tool_result" {
						last["content"] = append(blocks, block)
						return
					}
				}
			}
		}
		anthropicMessages = append(anthropicMessages, map[string]interface{}{
			"role":    "user",
			"content": []map[string]interface{}{block},
		})
	}

	for _, msg := range req.Messages {
		switch strings.ToLower(msg.Role) {
		case "system", "developer":
			// Anthropic carries the system prompt at the top level, not in messages.
			if txt := openAIMessageText(msg.Content); txt != "" {
				systemParts = append(systemParts, txt)
			}

		case "user":
			// Skip a turn that carries no non-empty content: Anthropic rejects empty
			// text blocks, and it merges consecutive same-role turns so dropping an
			// empty one cannot break alternation.
			blocks := openAIContentToAnthropicBlocks(msg.Content)
			if len(blocks) == 0 {
				continue
			}
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role":    "user",
				"content": blocks,
			})

		case "assistant":
			blocks := make([]map[string]interface{}, 0, 1+len(msg.ToolCalls))
			if txt := openAIMessageText(msg.Content); txt != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": txt})
			}
			for _, tc := range msg.ToolCalls {
				// OpenAI tool arguments are a JSON string; Anthropic input is an object.
				var input interface{} = map[string]interface{}{}
				if strings.TrimSpace(tc.Function.Arguments) != "" {
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
						input = map[string]interface{}{}
					}
				}
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			// Skip a wholly empty assistant turn rather than emit an empty text block
			// (which Anthropic rejects with a 400 and would trip account failover).
			if len(blocks) == 0 {
				continue
			}
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role":    "assistant",
				"content": blocks,
			})

		case "tool":
			// An OpenAI tool result maps to an Anthropic tool_result block whose
			// tool_use_id ties it back to the assistant's tool_use id.
			appendToolResult(map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": msg.ToolCallID,
				"content":     openAIMessageText(msg.Content),
			})

		default:
			// Unknown role: treat as user text so nothing is silently dropped.
			blocks := openAIContentToAnthropicBlocks(msg.Content)
			if len(blocks) == 0 {
				continue
			}
			anthropicMessages = append(anthropicMessages, map[string]interface{}{
				"role":    "user",
				"content": blocks,
			})
		}
	}

	out := map[string]interface{}{
		"messages": anthropicMessages,
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	// max_tokens is required by Anthropic; prefer max_tokens, then the newer
	// max_completion_tokens, then a default.
	switch {
	case req.MaxTokens != nil && *req.MaxTokens > 0:
		out["max_tokens"] = *req.MaxTokens
	case req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0:
		out["max_tokens"] = *req.MaxCompletionTokens
	default:
		out["max_tokens"] = bedrockOpenAIDefaultMaxTokens
	}
	if req.Temperature != nil {
		// OpenAI temperature ranges 0..2; Anthropic/Bedrock accepts 0..1 and 400s
		// otherwise. Clamp so a valid OpenAI request never turns into a pre-stream
		// Bedrock error that would fail over and cool down healthy accounts.
		t := *req.Temperature
		if t < 0 {
			t = 0
		} else if t > 1 {
			t = 1
		}
		out["temperature"] = t
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if seqs := normalizeStopSequences(req.Stop); len(seqs) > 0 {
		out["stop_sequences"] = seqs
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Function.Name == "" {
				continue
			}
			tool := map[string]interface{}{"name": t.Function.Name}
			if t.Function.Description != "" {
				tool["description"] = t.Function.Description
			}
			// Anthropic requires input_schema to be a JSON Schema object; default to
			// an open object when the OpenAI tool omits parameters.
			if len(t.Function.Parameters) > 0 {
				tool["input_schema"] = json.RawMessage(t.Function.Parameters)
			} else {
				tool["input_schema"] = map[string]interface{}{"type": "object"}
			}
			tools = append(tools, tool)
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if tc := openAIToolChoiceToAnthropic(req.ToolChoice); tc != nil {
				out["tool_choice"] = tc
			}
		}
	}

	return json.Marshal(out)
}

// normalizeStopSequences turns the OpenAI `stop` field (a string or an array of
// strings) into Anthropic's stop_sequences slice, dropping empties. Returns nil
// when absent or unusable so the caller omits the field.
func normalizeStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if strings.TrimSpace(one) == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		out := make([]string, 0, len(many))
		for _, s := range many {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// openAIToolChoiceToAnthropic maps an OpenAI tool_choice to Anthropic's form:
// "auto"->auto, "required"->any, {function:{name}}->{tool,name}. "none" and
// unrecognized values return nil so the default (auto) applies, avoiding a 400 on
// model versions that may not accept an explicit "none".
func openAIToolChoiceToAnthropic(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "required":
			return map[string]interface{}{"type": "any"}
		default: // "none" or unknown: leave default behavior
			return nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Function.Name != "" {
		return map[string]interface{}{"type": "tool", "name": obj.Function.Name}
	}
	return nil
}

// openAIMessageText flattens an OpenAI message content (string or content-part
// array) to plain text, concatenating text parts and ignoring non-text parts.
func openAIMessageText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var b strings.Builder
		for _, part := range v {
			pm, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			if pm["type"] == "text" {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// openAIContentToAnthropicBlocks converts an OpenAI user content value into
// Anthropic content blocks: string -> one text block; array -> text and image
// blocks (image_url data URLs become base64 image blocks). Empty text is dropped
// (Anthropic rejects empty text blocks); the result may be empty, in which case
// the caller skips the whole turn.
func openAIContentToAnthropicBlocks(content interface{}) []map[string]interface{} {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []map[string]interface{}{{"type": "text", "text": v}}
	case []interface{}:
		blocks := make([]map[string]interface{}, 0, len(v))
		for _, part := range v {
			pm, ok := part.(map[string]interface{})
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if s, ok := pm["text"].(string); ok && s != "" {
					blocks = append(blocks, map[string]interface{}{"type": "text", "text": s})
				}
			case "image_url":
				if img := openAIImageURLToAnthropicBlock(pm["image_url"]); img != nil {
					blocks = append(blocks, img)
				}
			}
		}
		return blocks
	default:
		return nil
	}
}

// openAIImageURLToAnthropicBlock converts an OpenAI image_url part into an
// Anthropic base64 image block. Only inline data URLs are supported (Bedrock
// invoke cannot fetch remote URLs); a non-data URL yields nil (skipped).
func openAIImageURLToAnthropicBlock(imageURL interface{}) map[string]interface{} {
	m, ok := imageURL.(map[string]interface{})
	if !ok {
		return nil
	}
	url, _ := m["url"].(string)
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	// Format: data:<media_type>;base64,<data>
	meta, data, found := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !found {
		return nil
	}
	mediaType := strings.TrimSuffix(meta, ";base64")
	if mediaType == "" {
		mediaType = "image/png"
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil
	}
	return map[string]interface{}{
		"type": "image",
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

// ---------------------------------------------------------------------------
// Non-stream response conversion: Anthropic Messages -> OpenAI chat.completion
// ---------------------------------------------------------------------------

// anthropicMessageResponse is the subset of a non-streaming Anthropic Messages
// response we translate.
type anthropicMessageResponse struct {
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// anthropicMessageToOpenAIResponse converts a non-streaming Anthropic Messages
// response body into an OpenAI chat.completion object and returns the input and
// output token counts for billing.
func anthropicMessageToOpenAIResponse(respBody []byte, model string) (map[string]interface{}, int, int, error) {
	var r anthropicMessageResponse
	if err := json.Unmarshal(respBody, &r); err != nil {
		return nil, 0, 0, fmt.Errorf("bedrock openai: invalid upstream response: %w", err)
	}

	var textParts []string
	var reasoningParts []string
	var toolCalls []map[string]interface{}
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			textParts = append(textParts, c.Text)
		case "thinking":
			reasoningParts = append(reasoningParts, c.Thinking)
		case "tool_use":
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   c.ID,
				"type": "function",
				"function": map[string]string{
					"name":      c.Name,
					"arguments": args,
				},
			})
		}
	}

	message := map[string]interface{}{"role": "assistant"}
	finishReason := anthropicStopReasonToOpenAIFinish(r.StopReason)
	// OpenAI allows content alongside tool_calls, and Claude commonly emits prose
	// before a tool call. Preserve text/reasoning in both branches so the streaming
	// and non-streaming paths return the same content for the same response.
	if len(textParts) > 0 {
		message["content"] = strings.Join(textParts, "")
	} else if len(toolCalls) > 0 {
		message["content"] = nil
	} else {
		message["content"] = ""
	}
	if len(reasoningParts) > 0 {
		message["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if finishReason == "" {
			finishReason = "tool_calls"
		}
	} else if finishReason == "" {
		finishReason = "stop"
	}

	resp := map[string]interface{}{
		"id":      "chatcmpl-" + uuid.New().String(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens":     r.Usage.InputTokens,
			"completion_tokens": r.Usage.OutputTokens,
			"total_tokens":      r.Usage.InputTokens + r.Usage.OutputTokens,
		},
	}
	return resp, r.Usage.InputTokens, r.Usage.OutputTokens, nil
}

// ---------------------------------------------------------------------------
// Streaming response conversion: Anthropic events -> chat.completion.chunk SSE
// ---------------------------------------------------------------------------

// anthropicStreamEvent is the subset of an Anthropic streaming event we read to
// drive the OpenAI chunk conversion.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// bedrockOpenAIStreamConv converts a sequence of native Anthropic streaming
// events into OpenAI chat.completion.chunk SSE frames. It is decoupled from the
// HTTP invoke so it can be unit-tested against a synthetic event sequence.
type bedrockOpenAIStreamConv struct {
	chatID string
	model  string

	toolIndex      int  // next OpenAI tool_calls index to assign
	curBlockIsTool bool // whether the open content block is a tool_use

	inputTokens  int
	outputTokens int
	finishReason string
	started      bool // at least one chunk was written to the client
}

// newBedrockOpenAIStreamConv creates a converter with a stable stream id.
func newBedrockOpenAIStreamConv(model string) *bedrockOpenAIStreamConv {
	return &bedrockOpenAIStreamConv{
		chatID: "chatcmpl-" + uuid.New().String(),
		model:  model,
	}
}

// chunkEnvelope returns a chat.completion.chunk with the given delta and
// finish_reason, matching the shape handleOpenAIStream already emits.
func (c *bedrockOpenAIStreamConv) chunkEnvelope(delta map[string]interface{}, finish interface{}) map[string]interface{} {
	return map[string]interface{}{
		"id":      c.chatID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   c.model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}

// writeChunk marshals and writes one SSE chunk, flushing when a flusher is set.
// A write error (client disconnect) is returned so the read loop can stop.
func (c *bedrockOpenAIStreamConv) writeChunk(w io.Writer, flusher http.Flusher, chunk map[string]interface{}) error {
	data, _ := json.Marshal(chunk)
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	c.started = true
	return nil
}

// onEvent processes one Anthropic event JSON and emits the matching OpenAI
// chunk(s). Usage-only events (message_start, message_delta) update counters
// without writing, so a failure before any content still allows failover.
func (c *bedrockOpenAIStreamConv) onEvent(w io.Writer, flusher http.Flusher, anthropicJSON []byte) error {
	var e anthropicStreamEvent
	if err := json.Unmarshal(anthropicJSON, &e); err != nil {
		return nil // ignore unparseable pings/keepalives rather than aborting
	}

	switch e.Type {
	case "message_start":
		if e.Message != nil && e.Message.Usage != nil && e.Message.Usage.InputTokens > 0 {
			c.inputTokens = e.Message.Usage.InputTokens
		}

	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "tool_use" {
			c.curBlockIsTool = true
			// First chunk for a tool call carries id + name + empty arguments.
			return c.writeChunk(w, flusher, c.chunkEnvelope(map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": c.toolIndex,
					"id":    e.ContentBlock.ID,
					"type":  "function",
					"function": map[string]string{
						"name":      e.ContentBlock.Name,
						"arguments": "",
					},
				}},
			}, nil))
		}
		c.curBlockIsTool = false

	case "content_block_delta":
		if e.Delta == nil {
			return nil
		}
		switch e.Delta.Type {
		case "text_delta":
			if e.Delta.Text == "" {
				return nil
			}
			return c.writeChunk(w, flusher, c.chunkEnvelope(map[string]interface{}{"content": e.Delta.Text}, nil))
		case "input_json_delta":
			// Partial tool arguments attach to the current tool call by index.
			return c.writeChunk(w, flusher, c.chunkEnvelope(map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index":    c.toolIndex,
					"function": map[string]string{"arguments": e.Delta.PartialJSON},
				}},
			}, nil))
		case "thinking_delta":
			if e.Delta.Thinking == "" {
				return nil
			}
			return c.writeChunk(w, flusher, c.chunkEnvelope(map[string]interface{}{"reasoning_content": e.Delta.Thinking}, nil))
		}

	case "content_block_stop":
		// Advance the tool index only after a tool block fully closes so the next
		// tool call gets a fresh OpenAI tool_calls index.
		if c.curBlockIsTool {
			c.toolIndex++
			c.curBlockIsTool = false
		}

	case "message_delta":
		if e.Delta != nil && e.Delta.StopReason != "" {
			c.finishReason = anthropicStopReasonToOpenAIFinish(e.Delta.StopReason)
		}
		if e.Usage != nil && e.Usage.OutputTokens > 0 {
			c.outputTokens = e.Usage.OutputTokens
		}

	case "message_stop":
		// Terminal handling is done by finish() so usage is included once.
	}
	return nil
}

// finish emits the terminal chunk (finish_reason + usage) and the [DONE]
// sentinel that OpenAI streaming clients expect.
func (c *bedrockOpenAIStreamConv) finish(w io.Writer, flusher http.Flusher) {
	finish := c.finishReason
	if finish == "" {
		finish = "stop"
	}
	chunk := c.chunkEnvelope(map[string]interface{}{}, finish)
	chunk["usage"] = map[string]int{
		"prompt_tokens":     c.inputTokens,
		"completion_tokens": c.outputTokens,
		"total_tokens":      c.inputTokens + c.outputTokens,
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// ---------------------------------------------------------------------------
// Invoke entrypoints (called from the OpenAI handlers' Bedrock dispatch branch)
// ---------------------------------------------------------------------------

// invokeBedrockOpenAIStream converts the OpenAI request to Anthropic, invokes
// Bedrock streaming, and re-emits OpenAI chunk SSE. Contract mirrors
// invokeBedrockStream: returns an error before any client bytes (failover), or
// nil once the response has been at least partially streamed.
func (h *Handler) invokeBedrockOpenAIStream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	// Non-Anthropic models are served via the Converse API instead of native invoke.
	if accountUsesConverse(p.account) {
		return h.invokeBedrockConverseOpenAIStream(w, flusher, p)
	}
	reqStart := time.Now()

	anthropicBody, err := openAIToAnthropicMessages(p.body)
	if err != nil {
		return err // pre-stream: safe to fail over
	}

	resp, err := h.doBedrockInvoke(p, anthropicBody, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	conv := newBedrockOpenAIStreamConv(p.model)
	streamErr := readBedrockEventStream(resp.Body, func(_ string, anthropicJSON []byte) error {
		return conv.onEvent(w, flusher, anthropicJSON)
	})

	if streamErr != nil && !conv.started {
		// Failed before any client bytes -> allow account failover.
		return streamErr
	}
	if streamErr != nil {
		logger.Warnf("[Bedrock] openai stream ended with error after partial output (account %s): %v", p.account.ID, streamErr)
	}

	conv.finish(w, flusher)
	h.recordBedrockSuccess(p, conv.inputTokens, conv.outputTokens, reqStart)
	return nil
}

// invokeBedrockOpenAINonStream converts the OpenAI request to Anthropic, invokes
// Bedrock non-streaming, converts the Anthropic response to an OpenAI
// chat.completion, and bills the customer key.
func (h *Handler) invokeBedrockOpenAINonStream(w http.ResponseWriter, p forwardParams) error {
	// Non-Anthropic models are served via the Converse API instead of native invoke.
	if accountUsesConverse(p.account) {
		return h.invokeBedrockConverseOpenAINonStream(w, p)
	}
	reqStart := time.Now()

	anthropicBody, err := openAIToAnthropicMessages(p.body)
	if err != nil {
		return err
	}

	resp, err := h.doBedrockInvoke(p, anthropicBody, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	openaiResp, inTok, outTok, err := anthropicMessageToOpenAIResponse(respBody, p.model)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(openaiResp)

	h.recordBedrockSuccess(p, inTok, outTok, reqStart)
	return nil
}
