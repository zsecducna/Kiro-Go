package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

func parseResponsesInput(raw json.RawMessage) ([]OpenAIMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("invalid input string: %w", err)
		}
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []OpenAIMessage{{Role: "user", Content: s}}, nil
	}

	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("invalid input array: %w", err)
		}
		return convertResponsesInputItems(items)
	}

	if trimmed[0] == '{' {
		return convertResponsesInputItems([]json.RawMessage{raw})
	}

	return nil, fmt.Errorf("unsupported input shape")
}

// extractResponsesInputTools reads the additional_tools item emitted by Codex
// Desktop. Recent Codex clients place their per-session tool declarations in
// the Responses API input array instead of the top-level tools field.
func extractResponsesInputTools(raw json.RawMessage) ([]OpenAITool, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed[0] == '"' {
		return nil, nil
	}

	var items []json.RawMessage
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("invalid input array: %w", err)
		}
	case '{':
		items = []json.RawMessage{raw}
	default:
		return nil, nil
	}

	var tools []OpenAITool
	for _, item := range items {
		var envelope struct {
			Type  string       `json:"type"`
			Tools []OpenAITool `json:"tools"`
		}
		if err := json.Unmarshal(item, &envelope); err != nil {
			continue
		}
		if envelope.Type == "additional_tools" {
			tools = append(tools, envelope.Tools...)
		}
	}
	return tools, nil
}

func mergeResponsesTools(primary, additional []OpenAITool) []OpenAITool {
	if len(additional) == 0 {
		return primary
	}

	merged := make([]OpenAITool, 0, len(primary)+len(additional))
	seen := make(map[string]bool, len(primary)+len(additional))
	appendUnique := func(tool OpenAITool) {
		key := strings.ToLower(strings.TrimSpace(tool.Type)) + "\x00" +
			strings.ToLower(strings.TrimSpace(tool.Function.Name))
		if seen[key] {
			return
		}
		seen[key] = true
		merged = append(merged, tool)
	}
	for _, tool := range primary {
		appendUnique(tool)
	}
	for _, tool := range additional {
		appendUnique(tool)
	}
	return merged
}

func convertResponsesInputItems(items []json.RawMessage) ([]OpenAIMessage, error) {
	messages := make([]OpenAIMessage, 0, len(items))
	pendingUserParts := []interface{}{}

	flushPendingUser := func() {
		if len(pendingUserParts) == 0 {
			return
		}
		messages = append(messages, OpenAIMessage{
			Role:    "user",
			Content: pendingUserParts,
		})
		pendingUserParts = nil
	}

	for _, item := range items {
		var obj map[string]interface{}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}

		typ, _ := obj["type"].(string)
		role, _ := obj["role"].(string)

		switch {
		case typ == "message" || (typ == "" && role != ""):
			flushPendingUser()
			msg := buildMessageFromInputItem(obj, role)
			if msg != nil {
				messages = append(messages, *msg)
			}

		case typ == "function_call_output" || typ == "custom_tool_call_output" || typ == "tool_result":
			flushPendingUser()
			callID, _ := obj["call_id"].(string)
			if callID == "" {
				callID, _ = obj["tool_call_id"].(string)
			}
			out := stringifyResponseToolOutput(obj["output"])
			if out == "" {
				out = stringifyResponseToolOutput(obj["content"])
			}
			messages = append(messages, OpenAIMessage{
				Role:       "tool",
				Content:    out,
				ToolCallID: callID,
			})

		case typ == "function_call" || typ == "custom_tool_call":
			flushPendingUser()
			tc := ToolCall{
				ID:   stringField(obj, "call_id", "id"),
				Type: "function",
			}
			tc.Function.Name, _ = obj["name"].(string)
			if typ == "custom_tool_call" {
				rawInput := stringifyArbitrary(obj["input"])
				wrapped, _ := json.Marshal(map[string]string{customToolInputField: rawInput})
				tc.Function.Arguments = string(wrapped)
			} else {
				tc.Function.Arguments = stringifyArbitrary(obj["arguments"])
			}
			appendResponsesToolCall(&messages, tc)

		case typ == "input_text" || typ == "text":
			text, _ := obj["text"].(string)
			if text != "" {
				pendingUserParts = append(pendingUserParts, map[string]interface{}{
					"type": "input_text",
					"text": text,
				})
			}

		case typ == "input_image", typ == "image", typ == "image_url":
			pendingUserParts = append(pendingUserParts, map[string]interface{}(obj))

		case typ == "output_text":
			flushPendingUser()
			text, _ := obj["text"].(string)
			if text != "" {
				messages = append(messages, OpenAIMessage{Role: "assistant", Content: text})
			}

		default:
			if role != "" {
				flushPendingUser()
				msg := buildMessageFromInputItem(obj, role)
				if msg != nil {
					messages = append(messages, *msg)
				}
			}
		}
	}

	flushPendingUser()
	return messages, nil
}

func buildMessageFromInputItem(obj map[string]interface{}, role string) *OpenAIMessage {
	if role == "" {
		role = "user"
	}

	if content, ok := obj["content"]; ok {
		switch v := content.(type) {
		case string:
			return &OpenAIMessage{Role: role, Content: v}
		case []interface{}:
			parts := make([]interface{}, 0, len(v))
			textOnly := strings.Builder{}
			anyNonText := false
			for _, p := range v {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				ptype, _ := part["type"].(string)
				switch ptype {
				case "input_text", "text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "output_text":
					if t, ok := part["text"].(string); ok {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				case "input_image", "image", "image_url":
					anyNonText = true
					parts = append(parts, part)
				default:
					if t, ok := part["text"].(string); ok && t != "" {
						textOnly.WriteString(t)
						parts = append(parts, map[string]interface{}{"type": "input_text", "text": t})
					}
				}
			}
			if !anyNonText {
				return &OpenAIMessage{Role: role, Content: textOnly.String()}
			}
			return &OpenAIMessage{Role: role, Content: parts}
		case map[string]interface{}:
			return buildMessageFromInputItem(v, role)
		}
	}

	if text, ok := obj["text"].(string); ok && text != "" {
		return &OpenAIMessage{Role: role, Content: text}
	}

	return nil
}

func stringifyArbitrary(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func stringifyResponseToolOutput(v interface{}) string {
	switch value := v.(type) {
	case []interface{}:
		var text strings.Builder
		for _, item := range value {
			switch part := item.(type) {
			case string:
				text.WriteString(part)
			case map[string]interface{}:
				if partText, ok := part["text"].(string); ok {
					text.WriteString(partText)
				}
			}
		}
		if text.Len() > 0 {
			return text.String()
		}
	}
	return stringifyArbitrary(v)
}

// appendResponsesToolCall keeps parallel Responses API calls in one assistant
// message so Kiro sees one tool-use turn followed by the matching results.
func appendResponsesToolCall(messages *[]OpenAIMessage, tc ToolCall) {
	if messages == nil {
		return
	}
	if n := len(*messages); n > 0 &&
		(*messages)[n-1].Role == "assistant" &&
		len((*messages)[n-1].ToolCalls) > 0 &&
		strings.TrimSpace(extractOpenAIMessageText((*messages)[n-1].Content)) == "" {
		(*messages)[n-1].ToolCalls = append((*messages)[n-1].ToolCalls, tc)
		return
	}
	*messages = append(*messages, OpenAIMessage{
		Role:      "assistant",
		Content:   "",
		ToolCalls: []ToolCall{tc},
	})
}

func stringField(obj map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := obj[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
