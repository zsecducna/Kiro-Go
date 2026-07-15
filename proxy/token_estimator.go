package proxy

import (
	"encoding/json"
	"math"
)

// tokenWeights holds the characters-per-token divisors used to approximate a
// tokenizer without running one, per character class.
type tokenWeights struct {
	regularAscii float64
	digits       float64
	symbols      float64
	nonASCII     float64
}

// reportingTokenWeights approximate Claude's tokenizer for usage reporting.
// They sit at the optimistic end of each class's real range, which is fine for
// a usage number.
var reportingTokenWeights = tokenWeights{regularAscii: 4.5, digits: 2.0, symbols: 1.5, nonASCII: 1.5}

// wireTokenWeights are deliberately pessimistic and are used only to bound what
// we put on the wire. The optimistic reporting weights are wrong in the unsafe
// direction for a ceiling: understating tokens lets an oversized request through
// and upstream rejects the whole call. The two classes that matter:
//   - nonASCII at 1.5 chars/token assumes CJK is cheaper than it is; real Claude
//     tokenization runs ~1-1.5 tokens per CJK character, so 1.0 is the safe floor.
//   - regularAscii at 4.5 chars/token is above the ~3.8-4.3 prose range.
//
// Being conservative here costs a little retained history; being optimistic
// costs the request.
var wireTokenWeights = tokenWeights{regularAscii: 4.0, digits: 2.0, symbols: 1.5, nonASCII: 1.0}

// estimateApproxTokens approximates the tokens in text for usage reporting.
func estimateApproxTokens(text string) int {
	return estimateApproxTokensWith(text, reportingTokenWeights)
}

// estimateWireTokens approximates the tokens in text for bounding a forwarded
// request, erring toward overcounting so the bound stays safe.
func estimateWireTokens(text string) int {
	return estimateApproxTokensWith(text, wireTokenWeights)
}

func estimateApproxTokensWith(text string, w tokenWeights) int {
	if text == "" {
		return 0
	}

	runes := []rune(text)
	length := len(runes)
	if length == 0 {
		return 0
	}
	if length < 5 {
		return max(1, int(math.Ceil(float64(length)/3.0)))
	}

	var regularAscii, digits, symbols, nonASCII int
	for _, r := range runes {
		switch {
		case r >= 0x80:
			nonASCII++
		case r >= '0' && r <= '9':
			digits++
		case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') || (r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
			symbols++
		default:
			regularAscii++
		}
	}

	estimated := int(math.Ceil(
		float64(regularAscii)/w.regularAscii +
			float64(digits)/w.digits +
			float64(symbols)/w.symbols +
			float64(nonASCII)/w.nonASCII,
	))

	if estimated < 1 {
		return 1
	}
	return estimated
}

// kiroImageTokenEstimate is a flat per-image token cost used when sizing a Kiro
// payload. An image's real token cost depends on its pixel dimensions, which we
// no longer have once it is base64-encoded; estimating the base64 text instead
// would overstate the cost by orders of magnitude. This assumes the high end of
// Claude's per-image cost so the estimate errs toward truncating.
const kiroImageTokenEstimate = 1600

// estimateKiroPayloadTokens approximates the input tokens Kiro charges for a
// converted payload: the current message plus every retained history entry.
// This is what bounds a forwarded request against the upstream model's context
// window, so it walks the real text fields rather than the serialized JSON
// (JSON punctuation is token-dense and would badly overstate the total).
func estimateKiroPayloadTokens(payload *KiroPayload) int {
	if payload == nil {
		return 0
	}

	total := estimateKiroUserInputTokens(&payload.ConversationState.CurrentMessage.UserInputMessage)
	for _, entry := range payload.ConversationState.History {
		total += estimateKiroHistoryEntryTokens(entry)
	}

	return total
}

// estimateKiroHistoryEntryTokens approximates the tokens for one history entry.
// An entry carries either a user turn or an assistant turn, never both.
func estimateKiroHistoryEntryTokens(entry KiroHistoryMessage) int {
	total := 0

	if entry.UserInputMessage != nil {
		total += estimateKiroUserInputTokens(entry.UserInputMessage)
	}

	if entry.AssistantResponseMessage != nil {
		total += estimateWireTokens(entry.AssistantResponseMessage.Content)
		for _, tu := range entry.AssistantResponseMessage.ToolUses {
			total += estimateWireTokens(tu.Name)
			total += estimateWireJSONTokens(tu.Input)
		}
	}

	return total
}

// estimateKiroUserInputTokens approximates the tokens for a single user turn:
// its text, attached images, tool specifications, and tool results.
func estimateKiroUserInputTokens(msg *KiroUserInputMessage) int {
	if msg == nil {
		return 0
	}

	total := estimateWireTokens(msg.Content)
	total += len(msg.Images) * kiroImageTokenEstimate

	if msgCtx := msg.UserInputMessageContext; msgCtx != nil {
		for _, tool := range msgCtx.Tools {
			spec := tool.ToolSpecification
			total += estimateWireTokens(spec.Name)
			total += estimateWireTokens(spec.Description)
			total += estimateWireJSONTokens(spec.InputSchema.JSON)
		}
		for _, tr := range msgCtx.ToolResults {
			for _, c := range tr.Content {
				total += estimateWireTokens(c.Text)
			}
		}
	}

	return total
}

// estimateWireJSONTokens is estimateJSONTokens with the conservative wire
// weights, for bounding a forwarded request.
func estimateWireJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateWireTokens(string(b))
}

func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	if req == nil {
		return 0
	}

	total := estimateClaudeValueTokens(req.System)

	for _, msg := range req.Messages {
		total += estimateClaudeValueTokens(msg.Content)
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Name)
		total += estimateApproxTokens(tool.Description)
		total += estimateJSONTokens(tool.InputSchema)
	}

	return total
}

func estimateClaudeOutputTokens(content, thinkingContent string, toolUses []KiroToolUse) int {
	total := estimateApproxTokens(content)
	total += estimateApproxTokens(thinkingContent)

	for _, tu := range toolUses {
		total += estimateApproxTokens(tu.Name)
		total += estimateJSONTokens(tu.Input)
	}

	return total
}

func estimateClaudeValueTokens(v interface{}) int {
	switch value := v.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	case []interface{}:
		total := 0
		for _, part := range value {
			total += estimateClaudeValueTokens(part)
		}
		return total
	case map[string]interface{}:
		typeName, _ := value["type"].(string)
		switch typeName {
		case "text":
			if text, ok := value["text"].(string); ok {
				return estimateApproxTokens(text)
			}
		case "thinking":
			if thinking, ok := value["thinking"].(string); ok {
				return estimateApproxTokens(thinking)
			}
		case "tool_use":
			total := 0
			if name, ok := value["name"].(string); ok {
				total += estimateApproxTokens(name)
			}
			if input, ok := value["input"]; ok {
				total += estimateJSONTokens(input)
			}
			if total > 0 {
				return total
			}
		case "tool_result":
			if content, ok := value["content"]; ok {
				return estimateClaudeValueTokens(content)
			}
		}

		total := 0
		if text, ok := value["text"].(string); ok {
			total += estimateApproxTokens(text)
		}
		if thinking, ok := value["thinking"].(string); ok {
			total += estimateApproxTokens(thinking)
		}
		if content, ok := value["content"]; ok {
			total += estimateClaudeValueTokens(content)
		}
		if total > 0 {
			return total
		}

		return estimateJSONTokens(value)
	default:
		return estimateJSONTokens(value)
	}
}

func estimateJSONTokens(v interface{}) int {
	if v == nil {
		return 0
	}

	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return estimateApproxTokens(string(b))
}

func estimateOpenAIRequestInputTokens(req *OpenAIRequest) int {
	if req == nil {
		return 0
	}

	total := 0

	for _, msg := range req.Messages {
		total += estimateOpenAIContentTokens(msg.Content)
		total += estimateApproxTokens(msg.ToolCallID)
		for _, tc := range msg.ToolCalls {
			total += estimateApproxTokens(tc.Function.Name)
			total += estimateApproxTokens(tc.Function.Arguments)
		}
	}

	for _, tool := range req.Tools {
		total += estimateApproxTokens(tool.Function.Name)
		total += estimateApproxTokens(tool.Function.Description)
		total += estimateJSONTokens(tool.Function.Parameters)
	}

	return total
}

func estimateOpenAIContentTokens(content interface{}) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return estimateApproxTokens(value)
	default:
		text := extractOpenAIMessageText(value)
		if text != "" {
			return estimateApproxTokens(text)
		}
		return estimateJSONTokens(value)
	}
}

func estimateOpenAIOutputTokens(content, reasoningContent string, toolUses []KiroToolUse) int {
	return estimateClaudeOutputTokens(content, reasoningContent, toolUses)
}
