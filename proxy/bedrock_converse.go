package proxy

// Bedrock Converse API variant for the native Bedrock provider.
//
// The default Bedrock path (bedrock.go) posts native Anthropic Messages to the
// invoke endpoint — the best fit for Claude models (zero translation for Anthropic
// customers). Non-Claude models (Nova, Llama, DeepSeek, ...) do NOT accept the
// Anthropic wire format; the portable path is Bedrock's unified Converse API
// (bedrock-runtime /converse and /converse-stream), reached with the SAME
// hand-rolled SigV4 signer but a different request/response schema.
//
// This file translates our internal canonical format (Anthropic Messages) to and
// from Converse, so both the Anthropic and OpenAI customer surfaces can resell
// non-Claude models. It is gated behind Account.BedrockUseConverse (default off)
// so Claude/native paths are never affected.
//
// Converse field names follow AWS's documented schema (and the reference logic in
// aws-samples/bedrock-access-gateway, MIT-0): request messages[].content[] with
// {text}/{image}/{toolUse}/{toolResult}; system:[{text}]; inferenceConfig with
// maxTokens/temperature/topP/stopSequences; toolConfig.tools[].toolSpec. Streaming
// events arrive as :event-type messageStart/contentBlockStart/contentBlockDelta/
// contentBlockStop/messageStop/metadata with the event JSON as the frame payload
// (NOT wrapped in the invoke path's {"bytes":...} envelope).
//
// NOTE: unit-verified against the documented schema and synthetic streams; the
// live wire has not yet been exercised against a real non-Claude model — validate
// end to end before production traffic.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"kiro-go/config"
	"kiro-go/logger"
)

// accountUsesConverse reports whether an account should use the Converse path.
func accountUsesConverse(account *config.Account) bool {
	return account != nil && account.BedrockUseConverse
}

// bedrockConverseEndpoint builds the Converse URL for a model id and stream flag.
func bedrockConverseEndpoint(region, modelID string, streaming bool) string {
	verb := "converse"
	if streaming {
		verb = "converse-stream"
	}
	return fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com/model/%s/%s", region, modelID, verb)
}

// ---------------------------------------------------------------------------
// Request conversion: Anthropic Messages -> Converse
// ---------------------------------------------------------------------------

// anthropicMessagesBody is the subset of an Anthropic Messages request we read to
// build a Converse request. system may be a string or an array of text blocks.
type anthropicMessagesBody struct {
	System        json.RawMessage   `json:"system"`
	Messages      []json.RawMessage `json:"messages"`
	MaxTokens     *int              `json:"max_tokens"`
	Temperature   *float64          `json:"temperature"`
	TopP          *float64          `json:"top_p"`
	StopSequences []string          `json:"stop_sequences"`
	Tools         []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// anthropicToConverseBody converts an Anthropic Messages body into a Converse
// request body. Content blocks map: text->{text}, image->{image}, tool_use->
// {toolUse}, tool_result->{toolResult}.
func anthropicToConverseBody(anthropicBody []byte) ([]byte, error) {
	var in anthropicMessagesBody
	if err := json.Unmarshal(anthropicBody, &in); err != nil {
		return nil, fmt.Errorf("bedrock converse: invalid anthropic body: %w", err)
	}

	messages := make([]map[string]interface{}, 0, len(in.Messages))
	for _, raw := range in.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		content := anthropicContentToConverse(m.Content)
		if len(content) == 0 {
			continue
		}
		messages = append(messages, map[string]interface{}{
			"role":    m.Role,
			"content": content,
		})
	}

	out := map[string]interface{}{"messages": messages}

	// system: Anthropic string or []text-block -> Converse [{text}].
	if sys := anthropicSystemToConverse(in.System); len(sys) > 0 {
		out["system"] = sys
	}

	inference := map[string]interface{}{}
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		inference["maxTokens"] = *in.MaxTokens
	}
	if in.Temperature != nil {
		inference["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		inference["topP"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		inference["stopSequences"] = in.StopSequences
	}
	if len(inference) > 0 {
		out["inferenceConfig"] = inference
	}

	if len(in.Tools) > 0 {
		tools := make([]map[string]interface{}, 0, len(in.Tools))
		for _, t := range in.Tools {
			if t.Name == "" {
				continue
			}
			schema := json.RawMessage(t.InputSchema)
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			spec := map[string]interface{}{
				"name":        t.Name,
				"inputSchema": map[string]interface{}{"json": schema},
			}
			if t.Description != "" {
				spec["description"] = t.Description
			}
			tools = append(tools, map[string]interface{}{"toolSpec": spec})
		}
		if len(tools) > 0 {
			toolConfig := map[string]interface{}{"tools": tools}
			if tc := anthropicToolChoiceToConverse(in.ToolChoice); tc != nil {
				toolConfig["toolChoice"] = tc
			}
			out["toolConfig"] = toolConfig
		}
	}

	return json.Marshal(out)
}

// anthropicToolChoiceToConverse maps an Anthropic tool_choice ({type:auto|any|
// tool,name}) to Converse toolConfig.toolChoice ({auto:{}}|{any:{}}|{tool:{name}}).
func anthropicToolChoiceToConverse(raw json.RawMessage) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return map[string]interface{}{"auto": map[string]interface{}{}}
	case "any":
		return map[string]interface{}{"any": map[string]interface{}{}}
	case "tool":
		if tc.Name != "" {
			return map[string]interface{}{"tool": map[string]interface{}{"name": tc.Name}}
		}
	}
	return nil
}

// anthropicSystemToConverse normalizes an Anthropic system field (string or array
// of {type:text,text}) into Converse system blocks [{text}].
func anthropicSystemToConverse(raw json.RawMessage) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []map[string]interface{}{{"text": s}}
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := make([]map[string]interface{}, 0, len(blocks))
		for _, b := range blocks {
			if t, ok := b["text"].(string); ok && t != "" {
				out = append(out, map[string]interface{}{"text": t})
			}
		}
		return out
	}
	return nil
}

// anthropicContentToConverse converts an Anthropic message content (string or
// block array) into Converse content blocks.
func anthropicContentToConverse(raw json.RawMessage) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []map[string]interface{}{{"text": s}}
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}

	out := make([]map[string]interface{}, 0, len(blocks))
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var text string
			_ = json.Unmarshal(b["text"], &text)
			if text != "" {
				out = append(out, map[string]interface{}{"text": text})
			}
		case "image":
			if img := anthropicImageToConverse(b["source"]); img != nil {
				out = append(out, img)
			}
		case "tool_use":
			var id, name string
			_ = json.Unmarshal(b["id"], &id)
			_ = json.Unmarshal(b["name"], &name)
			var input interface{} = map[string]interface{}{}
			if len(b["input"]) > 0 {
				_ = json.Unmarshal(b["input"], &input)
			}
			out = append(out, map[string]interface{}{
				"toolUse": map[string]interface{}{
					"toolUseId": id,
					"name":      name,
					"input":     input,
				},
			})
		case "tool_result":
			var toolUseID string
			_ = json.Unmarshal(b["tool_use_id"], &toolUseID)
			out = append(out, map[string]interface{}{
				"toolResult": map[string]interface{}{
					"toolUseId": toolUseID,
					"content":   anthropicToolResultContentToConverse(b["content"]),
				},
			})
		}
	}
	return out
}

// anthropicImageToConverse maps an Anthropic base64 image source to a Converse
// image block {image:{format, source:{bytes}}}.
func anthropicImageToConverse(rawSource json.RawMessage) map[string]interface{} {
	if len(rawSource) == 0 {
		return nil
	}
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	if err := json.Unmarshal(rawSource, &src); err != nil || src.Data == "" {
		return nil
	}
	format := strings.TrimPrefix(src.MediaType, "image/")
	if format == "" {
		format = "png"
	}
	return map[string]interface{}{
		"image": map[string]interface{}{
			"format": format,
			"source": map[string]interface{}{"bytes": src.Data},
		},
	}
}

// anthropicToolResultContentToConverse converts an Anthropic tool_result content
// (string or block array) into Converse toolResult content [{text}].
func anthropicToolResultContentToConverse(raw json.RawMessage) []map[string]interface{} {
	if len(raw) == 0 {
		return []map[string]interface{}{{"text": ""}}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []map[string]interface{}{{"text": s}}
	}
	var blocks []map[string]interface{}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		out := make([]map[string]interface{}, 0, len(blocks))
		for _, b := range blocks {
			if t, ok := b["text"].(string); ok {
				out = append(out, map[string]interface{}{"text": t})
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []map[string]interface{}{{"text": ""}}
}

// ---------------------------------------------------------------------------
// Non-stream response conversion: Converse -> Anthropic Messages
// ---------------------------------------------------------------------------

// converseResponse is the subset of a non-streaming Converse response we read.
type converseResponse struct {
	Output struct {
		Message struct {
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"usage"`
}

// converseResponseToAnthropicMessage converts a non-streaming Converse response
// into an Anthropic Messages response body (so downstream code — Anthropic
// passthrough or Anthropic->OpenAI — treats it identically to a native invoke).
func converseResponseToAnthropicMessage(converseBody []byte, model string) ([]byte, int, int, error) {
	var r converseResponse
	if err := json.Unmarshal(converseBody, &r); err != nil {
		return nil, 0, 0, fmt.Errorf("bedrock converse: invalid response: %w", err)
	}

	content := make([]map[string]interface{}, 0, len(r.Output.Message.Content))
	for _, c := range r.Output.Message.Content {
		switch {
		case len(c["text"]) > 0:
			var text string
			_ = json.Unmarshal(c["text"], &text)
			content = append(content, map[string]interface{}{"type": "text", "text": text})
		case len(c["toolUse"]) > 0:
			var tu struct {
				ToolUseID string          `json:"toolUseId"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(c["toolUse"], &tu)
			var input interface{} = map[string]interface{}{}
			if len(tu.Input) > 0 {
				_ = json.Unmarshal(tu.Input, &input)
			}
			content = append(content, map[string]interface{}{
				"type": "tool_use", "id": tu.ToolUseID, "name": tu.Name, "input": input,
			})
		case len(c["reasoningContent"]) > 0:
			var rc struct {
				ReasoningText struct {
					Text string `json:"text"`
				} `json:"reasoningText"`
			}
			_ = json.Unmarshal(c["reasoningContent"], &rc)
			if rc.ReasoningText.Text != "" {
				content = append(content, map[string]interface{}{"type": "thinking", "thinking": rc.ReasoningText.Text})
			}
		}
	}

	anthropic := map[string]interface{}{
		"id":          "msg_" + uuid.New().String(),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": converseStopReasonToAnthropic(r.StopReason),
		"usage": map[string]int{
			"input_tokens":  r.Usage.InputTokens,
			"output_tokens": r.Usage.OutputTokens,
		},
	}
	body, err := json.Marshal(anthropic)
	return body, r.Usage.InputTokens, r.Usage.OutputTokens, err
}

// converseStopReasonToAnthropic maps a Converse stopReason to the Anthropic
// stop_reason vocabulary (they mostly coincide).
func converseStopReasonToAnthropic(sr string) string {
	switch strings.ToLower(strings.TrimSpace(sr)) {
	case "", "end_turn":
		return "end_turn"
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	case "content_filtered", "guardrail_intervened":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

// ---------------------------------------------------------------------------
// Streaming: Converse event-stream -> Anthropic events
// ---------------------------------------------------------------------------

// readBedrockConverseEventStream reads AWS event-stream frames whose payload is
// the Converse event JSON directly (no {"bytes":...} envelope). It mirrors
// readBedrockEventStream's framing/exception handling but yields the raw payload
// keyed by the :event-type header (messageStart, contentBlockDelta, ...).
func readBedrockConverseEventStream(body io.Reader, onEvent func(eventType string, payload []byte) error) error {
	for {
		prelude := make([]byte, 12)
		if _, err := io.ReadFull(body, prelude); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			continue
		}
		if totalLength > maxEventStreamMessageBytes {
			return errEventStreamFrameTooLarge
		}

		msgBuf := make([]byte, totalLength-12)
		if _, err := io.ReadFull(body, msgBuf); err != nil {
			return err
		}
		if headersLength < 0 || headersLength > len(msgBuf)-4 {
			continue
		}

		headers := msgBuf[0:headersLength]
		payload := msgBuf[headersLength : len(msgBuf)-4]

		messageType := extractHeaderString(headers, ":message-type")
		eventType := extractEventType(headers)

		if messageType == "exception" || messageType == "error" ||
			eventType == "exception" || eventType == "error" ||
			extractHeaderString(headers, ":exception-type") != "" {
			return &bedrockStreamError{
				EventType: firstNonEmpty(extractHeaderString(headers, ":exception-type"), eventType, messageType),
				Message:   extractJSONMessage(payload),
			}
		}

		if len(payload) == 0 {
			continue
		}
		if err := onEvent(eventType, payload); err != nil {
			return err
		}
	}
}

// converseStreamConv converts Converse streaming events into native Anthropic
// streaming events, emitting each translated event through a callback so callers
// can either write Anthropic SSE (Anthropic customers) or feed the OpenAI stream
// converter (OpenAI customers). Converse emits metadata (usage) AFTER messageStop,
// so the terminal message_delta/message_stop is deferred to finalize().
type converseStreamConv struct {
	startedBlocks  map[int]bool
	stopReason     string
	inputTokens    int
	outputTokens   int
	sawMessageStop bool
	emittedAny     bool // any Anthropic event was emitted (used for EOF-without-stop handling)
}

func newConverseStreamConv() *converseStreamConv {
	return &converseStreamConv{startedBlocks: map[int]bool{}}
}

// emitFn writes one translated Anthropic event (raw JSON) to the client sink.
type emitFn func(anthropicJSON []byte) error

// process translates one Converse event and emits the resulting Anthropic
// event(s). Terminal events are held for finalize().
func (c *converseStreamConv) process(eventType string, payload []byte, emit emitFn) error {
	switch eventType {
	case "messageStart":
		c.emittedAny = true
		return emit([]byte(`{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","content":[],"usage":{"input_tokens":0,"output_tokens":0}}}`))

	case "contentBlockStart":
		var e struct {
			Index int `json:"contentBlockIndex"`
			Start struct {
				ToolUse *struct {
					ToolUseID string `json:"toolUseId"`
					Name      string `json:"name"`
				} `json:"toolUse"`
			} `json:"start"`
		}
		_ = json.Unmarshal(payload, &e)
		if e.Start.ToolUse != nil {
			c.startedBlocks[e.Index] = true
			ev, _ := json.Marshal(map[string]interface{}{
				"type":  "content_block_start",
				"index": e.Index,
				"content_block": map[string]interface{}{
					"type": "tool_use", "id": e.Start.ToolUse.ToolUseID, "name": e.Start.ToolUse.Name, "input": map[string]interface{}{},
				},
			})
			return emit(ev)
		}
		return nil

	case "contentBlockDelta":
		var e struct {
			Index int `json:"contentBlockIndex"`
			Delta struct {
				Text    *string `json:"text"`
				ToolUse *struct {
					Input string `json:"input"`
				} `json:"toolUse"`
				ReasoningContent *struct {
					Text string `json:"text"`
				} `json:"reasoningContent"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(payload, &e)
		kind := "text"
		switch {
		case e.Delta.ToolUse != nil:
			kind = "tool"
		case e.Delta.ReasoningContent != nil:
			kind = "thinking"
		}
		if err := c.ensureBlockStarted(e.Index, kind, emit); err != nil {
			return err
		}
		switch {
		case e.Delta.Text != nil:
			ev, _ := json.Marshal(map[string]interface{}{
				"type": "content_block_delta", "index": e.Index,
				"delta": map[string]interface{}{"type": "text_delta", "text": *e.Delta.Text},
			})
			return emit(ev)
		case e.Delta.ToolUse != nil:
			ev, _ := json.Marshal(map[string]interface{}{
				"type": "content_block_delta", "index": e.Index,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": e.Delta.ToolUse.Input},
			})
			return emit(ev)
		case e.Delta.ReasoningContent != nil:
			ev, _ := json.Marshal(map[string]interface{}{
				"type": "content_block_delta", "index": e.Index,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": e.Delta.ReasoningContent.Text},
			})
			return emit(ev)
		}
		return nil

	case "contentBlockStop":
		var e struct {
			Index int `json:"contentBlockIndex"`
		}
		_ = json.Unmarshal(payload, &e)
		ev, _ := json.Marshal(map[string]interface{}{"type": "content_block_stop", "index": e.Index})
		return emit(ev)

	case "messageStop":
		var e struct {
			StopReason string `json:"stopReason"`
		}
		_ = json.Unmarshal(payload, &e)
		c.stopReason = e.StopReason
		c.sawMessageStop = true
		return nil // defer message_delta/message_stop to finalize (usage arrives in metadata)

	case "metadata":
		var e struct {
			Usage struct {
				InputTokens  int `json:"inputTokens"`
				OutputTokens int `json:"outputTokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(payload, &e)
		if e.Usage.InputTokens > 0 {
			c.inputTokens = e.Usage.InputTokens
		}
		if e.Usage.OutputTokens > 0 {
			c.outputTokens = e.Usage.OutputTokens
		}
		return nil
	}
	return nil
}

// ensureBlockStarted lazily emits a content_block_start the first time a delta
// arrives for a block Converse did not open with its own start event. The start's
// content_block type must match the delta kind ("text" or "thinking") so the
// emitted Anthropic stream is well-formed; tool blocks always get an explicit
// contentBlockStart from Converse, so they are skipped here.
func (c *converseStreamConv) ensureBlockStarted(index int, kind string, emit emitFn) error {
	if kind == "tool" || c.startedBlocks[index] {
		return nil
	}
	c.startedBlocks[index] = true
	var block map[string]interface{}
	if kind == "thinking" {
		block = map[string]interface{}{"type": "thinking", "thinking": ""}
	} else {
		block = map[string]interface{}{"type": "text", "text": ""}
	}
	ev, _ := json.Marshal(map[string]interface{}{
		"type": "content_block_start", "index": index, "content_block": block,
	})
	return emit(ev)
}

// finalize emits the terminal message_delta (stop_reason + output usage) and
// message_stop once, after all Converse events (incl. trailing metadata) are seen.
func (c *converseStreamConv) finalize(emit emitFn) error {
	// Nothing streamed and no stop seen: nothing to terminate.
	if !c.sawMessageStop && !c.emittedAny {
		return nil
	}
	// If the upstream closed cleanly without a messageStop frame, still emit a
	// terminal so SSE consumers don't hang; stop_reason defaults to end_turn.
	delta, _ := json.Marshal(map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": converseStopReasonToAnthropic(c.stopReason)},
		"usage": map[string]int{"input_tokens": c.inputTokens, "output_tokens": c.outputTokens},
	})
	if err := emit(delta); err != nil {
		return err
	}
	return emit([]byte(`{"type":"message_stop"}`))
}

// ---------------------------------------------------------------------------
// Invoke helper
// ---------------------------------------------------------------------------

// doBedrockConverseInvoke builds, signs, and sends a Converse request. Mirrors
// doBedrockInvoke but targets the /converse[-stream] endpoint with a Converse
// body (no anthropic_version). All returned errors occur before any client bytes.
func (h *Handler) doBedrockConverseInvoke(p forwardParams, converseBody []byte, streaming bool) (*http.Response, error) {
	modelID, err := resolveBedrockModelID(p.account, p.model)
	if err != nil {
		return nil, err
	}
	creds, err := bedrockCredsFor(p.account)
	if err != nil {
		return nil, err
	}
	region := bedrockRegionFor(p.account)

	rawURL := bedrockConverseEndpoint(region, modelID, streaming)
	req, err := newBedrockRequestForURL(rawURL, converseBody)
	if err != nil {
		return nil, err
	}
	if streaming {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	signSigV4(req, converseBody, creds, region, bedrockService, time.Now())

	resp, err := bedrockHTTPClient(p.account).Do(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock: request failed: %w", err)
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Entrypoints (called from the Bedrock dispatch when accountUsesConverse)
// ---------------------------------------------------------------------------

// invokeBedrockConverseAnthropicNonStream serves an Anthropic-format request via
// Converse, writing the Anthropic Messages JSON response back to the client.
func (h *Handler) invokeBedrockConverseAnthropicNonStream(w http.ResponseWriter, p forwardParams) error {
	reqStart := time.Now()
	converseBody, err := anthropicToConverseBody(p.body)
	if err != nil {
		return err
	}
	resp, err := h.doBedrockConverseInvoke(p, converseBody, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	anthropicBody, inTok, outTok, err := converseResponseToAnthropicMessage(respBody, p.model)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(anthropicBody)
	h.recordBedrockSuccess(p, inTok, outTok, reqStart)
	return nil
}

// invokeBedrockConverseAnthropicStream serves an Anthropic-format streaming
// request via Converse, re-emitting translated Anthropic SSE events.
func (h *Handler) invokeBedrockConverseAnthropicStream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	reqStart := time.Now()
	converseBody, err := anthropicToConverseBody(p.body)
	if err != nil {
		return err
	}
	resp, err := h.doBedrockConverseInvoke(p, converseBody, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	conv := newConverseStreamConv()
	var streamedAny bool
	emit := func(anthropicJSON []byte) error {
		evtName := innerEventType(anthropicJSON)
		streamedAny = true
		if _, werr := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evtName, anthropicJSON); werr != nil {
			return werr
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	streamErr := readBedrockConverseEventStream(resp.Body, func(eventType string, payload []byte) error {
		return conv.process(eventType, payload, emit)
	})
	if streamErr == nil {
		streamErr = conv.finalize(emit)
	}

	if streamErr != nil && !streamedAny {
		return streamErr
	}
	if streamErr != nil {
		logger.Warnf("[Bedrock] converse stream ended with error after partial output (account %s): %v", p.account.ID, streamErr)
	} else if conv.emittedAny && !conv.sawMessageStop {
		logger.Warnf("[Bedrock] converse stream closed without messageStop (account %s); emitted synthetic terminal", p.account.ID)
	}
	h.recordBedrockSuccess(p, conv.inputTokens, conv.outputTokens, reqStart)
	return nil
}

// invokeBedrockConverseOpenAINonStream serves an OpenAI-format request via
// Converse: OpenAI -> Anthropic -> Converse invoke -> Anthropic -> OpenAI.
func (h *Handler) invokeBedrockConverseOpenAINonStream(w http.ResponseWriter, p forwardParams) error {
	reqStart := time.Now()
	anthropicReq, err := openAIToAnthropicMessages(p.body)
	if err != nil {
		return err
	}
	converseBody, err := anthropicToConverseBody(anthropicReq)
	if err != nil {
		return err
	}
	resp, err := h.doBedrockConverseInvoke(p, converseBody, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	anthropicResp, _, _, err := converseResponseToAnthropicMessage(respBody, p.model)
	if err != nil {
		return err
	}
	openaiResp, inTok, outTok, err := anthropicMessageToOpenAIResponse(anthropicResp, p.model)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(openaiResp)
	h.recordBedrockSuccess(p, inTok, outTok, reqStart)
	return nil
}

// invokeBedrockConverseOpenAIStream serves an OpenAI-format streaming request via
// Converse, feeding translated Anthropic events into the OpenAI chunk converter.
func (h *Handler) invokeBedrockConverseOpenAIStream(w http.ResponseWriter, flusher http.Flusher, p forwardParams) error {
	reqStart := time.Now()
	anthropicReq, err := openAIToAnthropicMessages(p.body)
	if err != nil {
		return err
	}
	converseBody, err := anthropicToConverseBody(anthropicReq)
	if err != nil {
		return err
	}
	resp, err := h.doBedrockConverseInvoke(p, converseBody, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("bedrock: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	conv := newConverseStreamConv()
	oconv := newBedrockOpenAIStreamConv(p.model)
	emit := func(anthropicJSON []byte) error {
		return oconv.onEvent(w, flusher, anthropicJSON)
	}
	streamErr := readBedrockConverseEventStream(resp.Body, func(eventType string, payload []byte) error {
		return conv.process(eventType, payload, emit)
	})
	if streamErr == nil {
		streamErr = conv.finalize(emit)
	}
	// The OpenAI converter tracks its own usage from the translated Anthropic
	// events; prefer Converse metadata totals when present.
	if conv.inputTokens > 0 {
		oconv.inputTokens = conv.inputTokens
	}
	if conv.outputTokens > 0 {
		oconv.outputTokens = conv.outputTokens
	}

	if streamErr != nil && !oconv.started {
		return streamErr
	}
	if streamErr != nil {
		logger.Warnf("[Bedrock] converse openai stream ended with error after partial output (account %s): %v", p.account.ID, streamErr)
	} else if conv.emittedAny && !conv.sawMessageStop {
		logger.Warnf("[Bedrock] converse openai stream closed without messageStop (account %s); emitted synthetic terminal", p.account.ID)
	}
	oconv.finish(w, flusher)
	h.recordBedrockSuccess(p, oconv.inputTokens, oconv.outputTokens, reqStart)
	return nil
}
