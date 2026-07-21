package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type ReasoningSchemaPath string

const (
	ReasoningSchemaNone         ReasoningSchemaPath = ""
	ReasoningSchemaOutputConfig ReasoningSchemaPath = "output_config"
	ReasoningSchemaReasoning    ReasoningSchemaPath = "reasoning"
)

type ModelReasoningCapability struct {
	ModelID string

	SupportsThinking       bool
	ThinkingTypes          []string
	SupportsDisplay        bool
	SupportsBudgetTokens   bool
	EffortPath             ReasoningSchemaPath
	Efforts                []string
	SchemaParseError       string
}

func (c ModelReasoningCapability) SupportsNativeReasoning() bool {
	return c.SupportsThinking || c.EffortPath != ReasoningSchemaNone
}

func ParseModelReasoningCapability(model ModelInfo) ModelReasoningCapability {
	capability := ModelReasoningCapability{
		ModelID: model.ModelId,
	}

	root, err := decodeModelRequestSchema(
		model.AdditionalModelRequestFieldsSchema,
	)
	if err != nil {
		capability.SchemaParseError = err.Error()
		return capability
	}
	if root == nil {
		return capability
	}

	root = unwrapAdditionalFieldsSchema(root)
	properties := schemaProperties(root)
	if properties == nil {
		return capability
	}

	if thinkingSchema := schemaObject(properties["thinking"]); thinkingSchema != nil {
		capability.SupportsThinking = true

		thinkingProps := schemaProperties(thinkingSchema)
		capability.ThinkingTypes = enumStrings(
			schemaObject(thinkingProps["type"]),
		)
		capability.SupportsDisplay =
			thinkingProps != nil && thinkingProps["display"] != nil
		capability.SupportsBudgetTokens =
			thinkingProps != nil && thinkingProps["budget_tokens"] != nil
	}

	if outputConfigSchema := schemaObject(properties["output_config"]); outputConfigSchema != nil {
		outputProps := schemaProperties(outputConfigSchema)
		efforts := enumStrings(schemaObject(outputProps["effort"]))
		if len(efforts) > 0 {
			capability.EffortPath = ReasoningSchemaOutputConfig
			capability.Efforts = normalizeEffortList(efforts)
			return capability
		}
	}

	if reasoningSchema := schemaObject(properties["reasoning"]); reasoningSchema != nil {
		reasoningProps := schemaProperties(reasoningSchema)
		efforts := enumStrings(schemaObject(reasoningProps["effort"]))
		if len(efforts) > 0 {
			capability.EffortPath = ReasoningSchemaReasoning
			capability.Efforts = normalizeEffortList(efforts)
		}
	}

	return capability
}

func decodeModelRequestSchema(raw json.RawMessage) (map[string]interface{}, error) {
	data := bytes.TrimSpace(raw)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}

	for depth := 0; depth < 3; depth++ {
		if len(data) == 0 {
			return nil, nil
		}

		if data[0] == '"' {
			var encoded string
			if err := json.Unmarshal(data, &encoded); err != nil {
				return nil, fmt.Errorf("decode schema string: %w", err)
			}
			data = []byte(encoded)
			continue
		}

		var result map[string]interface{}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, fmt.Errorf("decode schema object: %w", err)
		}
		return result, nil
	}

	return nil, fmt.Errorf("schema nesting is too deep")
}


func unwrapAdditionalFieldsSchema(
	root map[string]interface{},
) map[string]interface{} {
	for _, key := range []string{"schema", "jsonSchema"} {
		if nested := schemaObject(root[key]); nested != nil {
			root = nested
		}
	}

	properties := schemaProperties(root)
	if properties == nil {
		return root
	}

	if nested := schemaObject(properties["additionalModelRequestFields"]); nested != nil {
		return nested
	}

	return root
}

func schemaProperties(
	schema map[string]interface{},
) map[string]interface{} {
	if schema == nil {
		return nil
	}

	properties, _ := schema["properties"].(map[string]interface{})
	return properties
}

func schemaObject(value interface{}) map[string]interface{} {
	object, _ := value.(map[string]interface{})
	return object
}

func enumStrings(schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}

	if constant, ok := schema["const"].(string); ok {
		return []string{constant}
	}

	rawValues, ok := schema["enum"].([]interface{})
	if !ok {
		return nil
	}

	values := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		value, ok := rawValue.(string)
		if !ok {
			continue
		}

		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}

	return values
}

var canonicalEffortOrder = []string{
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
	"max",
}

func canonicalEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))

	switch value {
	case "extra_high", "extra-high":
		return "xhigh"
	default:
		return value
	}
}

func normalizeEffortList(values []string) []string {
	byCanonical := make(map[string]string)

	for _, value := range values {
		canonical := canonicalEffort(value)
		if canonical == "" {
			continue
		}
		if _, exists := byCanonical[canonical]; !exists {
			byCanonical[canonical] = value
		}
	}

	ordered := make([]string, 0, len(byCanonical))

	for _, canonical := range canonicalEffortOrder {
		if original, ok := byCanonical[canonical]; ok {
			ordered = append(ordered, original)
			delete(byCanonical, canonical)
		}
	}

	for _, original := range byCanonical {
		ordered = append(ordered, original)
	}

	return ordered
}

type reasoningIntent struct {
	Enabled      bool
	Disabled     bool
	ThinkingType string
	Display      string
	Effort       string
	BudgetTokens int
	MaxTokens    int
}

func BuildClaudeAdditionalModelRequestFields(
	req *ClaudeRequest,
	capability ModelReasoningCapability,
) (
	fields map[string]interface{},
	requested bool,
	err error,
) {
	if req == nil {
		return nil, false, nil
	}

	intent := reasoningIntent{
		MaxTokens: req.MaxTokens,
	}

	if req.Thinking != nil {
		intent.ThinkingType = strings.ToLower(
			strings.TrimSpace(req.Thinking.Type),
		)
		intent.Display = strings.ToLower(
			strings.TrimSpace(req.Thinking.Display),
		)
		intent.BudgetTokens = req.Thinking.BudgetTokens

		switch intent.ThinkingType {
		case "enabled", "adaptive":
			intent.Enabled = true
		case "disabled", "none":
			intent.Disabled = true
		}
	}

	if req.OutputConfig != nil {
		intent.Effort = req.OutputConfig.Effort
	}
	if strings.TrimSpace(intent.Effort) == "" {
		intent.Effort = req.ReasoningEffort
	}
	if strings.TrimSpace(intent.Effort) != "" {
		intent.Enabled = true
	}

	return buildAdditionalModelRequestFields(intent, capability)
}

func BuildOpenAIAdditionalModelRequestFields(
	req *OpenAIRequest,
	capability ModelReasoningCapability,
) (
	fields map[string]interface{},
	requested bool,
	err error,
) {
	if req == nil {
		return nil, false, nil
	}

	intent := reasoningIntent{
		MaxTokens: req.MaxTokens,
	}

	if req.Thinking != nil {
		intent.ThinkingType = strings.ToLower(
			strings.TrimSpace(req.Thinking.Type),
		)
		intent.Display = strings.ToLower(
			strings.TrimSpace(req.Thinking.Display),
		)
		intent.BudgetTokens = req.Thinking.BudgetTokens

		switch intent.ThinkingType {
		case "enabled", "adaptive":
			intent.Enabled = true
		case "disabled", "none":
			intent.Disabled = true
		}
	}

	if req.OutputConfig != nil {
		intent.Effort = req.OutputConfig.Effort
	}
	if strings.TrimSpace(intent.Effort) == "" && req.Reasoning != nil {
		intent.Effort = req.Reasoning.Effort
	}
	if strings.TrimSpace(intent.Effort) == "" {
		intent.Effort = req.ReasoningEffort
	}
	if strings.TrimSpace(intent.Effort) != "" {
		intent.Enabled = true
	}

	return buildAdditionalModelRequestFields(intent, capability)
}

func buildAdditionalModelRequestFields(
	intent reasoningIntent,
	capability ModelReasoningCapability,
) (
	fields map[string]interface{},
	requested bool,
	err error,
) {
	if intent.Disabled {
		if strings.TrimSpace(intent.Effort) != "" {
			return nil, false, fmt.Errorf(
				"thinking is disabled but reasoning effort was also provided",
			)
		}
		return nil, false, nil
	}

	requested = intent.Enabled
	if !requested {
		return nil, false, nil
	}

	if !capability.SupportsNativeReasoning() {
		return nil, true, nil
	}

	fields = make(map[string]interface{})

	if capability.SupportsThinking {
		thinking := make(map[string]interface{})

		thinkingType := selectThinkingType(
			intent.ThinkingType,
			capability.ThinkingTypes,
		)
		if thinkingType != "" {
			thinking["type"] = thinkingType
		}

		if capability.SupportsDisplay && intent.Display != "" {
			switch intent.Display {
			case "summarized", "omitted":
				thinking["display"] = intent.Display
			default:
				return nil, true, fmt.Errorf(
					"unsupported thinking.display %q",
					intent.Display,
				)
			}
		}

		if capability.SupportsBudgetTokens &&
			intent.BudgetTokens > 0 {
			thinking["budget_tokens"] = intent.BudgetTokens
		}

		if len(thinking) > 0 {
			fields["thinking"] = thinking
		}
	}

	effort := strings.TrimSpace(intent.Effort)

	if effort == "" &&
		intent.BudgetTokens > 0 &&
		len(capability.Efforts) > 0 {
		effort = effortFromBudget(
			intent.BudgetTokens,
			intent.MaxTokens,
			capability.Efforts,
		)
	}

	if effort != "" {
		supportedEffort, ok := matchSupportedEffort(
			effort,
			capability.Efforts,
		)
		if !ok {
			return nil, true, fmt.Errorf(
				"model %s does not support reasoning effort %q; supported: %s",
				capability.ModelID,
				effort,
				strings.Join(capability.Efforts, ", "),
			)
		}

		switch capability.EffortPath {
		case ReasoningSchemaOutputConfig:
			fields["output_config"] = map[string]interface{}{
				"effort": supportedEffort,
			}

		case ReasoningSchemaReasoning:
			fields["reasoning"] = map[string]interface{}{
				"effort": supportedEffort,
			}

		default:
			return nil, true, fmt.Errorf(
				"model %s does not expose a configurable effort field",
				capability.ModelID,
			)
		}
	}

	if len(fields) == 0 {
		return nil, true, nil
	}

	return fields, true, nil
}

func selectThinkingType(
	requested string,
	supported []string,
) string {
	requested = strings.ToLower(strings.TrimSpace(requested))

	if len(supported) == 0 {
		if requested == "enabled" || requested == "adaptive" {
			return requested
		}
		return "adaptive"
	}

	for _, supportedType := range supported {
		if strings.EqualFold(supportedType, requested) {
			return supportedType
		}
	}

	for _, preferred := range []string{"adaptive", "enabled"} {
		for _, supportedType := range supported {
			if strings.EqualFold(supportedType, preferred) {
				return supportedType
			}
		}
	}

	return supported[0]
}

func matchSupportedEffort(
	requested string,
	supported []string,
) (string, bool) {
	requestedCanonical := canonicalEffort(requested)

	for _, supportedEffort := range supported {
		if canonicalEffort(supportedEffort) == requestedCanonical {
			return supportedEffort, true
		}
	}

	return "", false
}

func effortFromBudget(
	budgetTokens int,
	maxTokens int,
	supported []string,
) string {
	if len(supported) == 0 {
		return ""
	}

	if maxTokens <= 0 || budgetTokens <= 0 {
		return supported[len(supported)/2]
	}

	ratio := float64(budgetTokens) / float64(maxTokens)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	index := int(math.Ceil(ratio*float64(len(supported)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(supported) {
		index = len(supported) - 1
	}

	return supported[index]
}

func reasoningFieldsJSON(
	fields map[string]interface{},
) string {
	if len(fields) == 0 {
		return "{}"
	}

	data, err := json.Marshal(fields)
	if err != nil {
		return fmt.Sprintf(
			`{"marshalError":%q}`,
			err.Error(),
		)
	}

	return string(data)
}