package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
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

	SupportsThinking     bool
	ThinkingTypes        []string
	SupportsDisplay      bool
	ThinkingDisplays     []string
	SupportsBudgetTokens bool
	EffortPath           ReasoningSchemaPath
	Efforts              []string
	SchemaParseError     string
}

func (c ModelReasoningCapability) SupportsNativeReasoning() bool {
	return c.SupportsThinking || c.EffortPath != ReasoningSchemaNone
}

// Parse thinking and effort support from the model schema.
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
		displaySchema := schemaObject(thinkingProps["display"])

		capability.SupportsDisplay = displaySchema != nil
		capability.ThinkingDisplays = enumStrings(displaySchema)
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

// Normalize aliases while preserving schema-provided values.
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

	unknown := make([]string, 0, len(byCanonical))

	for _, original := range byCanonical {
		unknown = append(unknown, original)
	}
	sort.Strings(unknown)
	ordered = append(ordered, unknown...)

	return ordered
}

type reasoningIntent struct {
	Enabled      bool
	Disabled     bool
	ThinkingType string
	Display      string
	Effort       string
	BudgetTokens int
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

	intent := reasoningIntent{}

	if req.Thinking != nil {
		intent.ThinkingType = normalizeClaudeThinkingType(req.Thinking.Type, capability.ThinkingTypes)
		intent.Display = strings.ToLower(strings.TrimSpace(req.Thinking.Display))
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

	intent := reasoningIntent{}

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

	effort := canonicalEffort(intent.Effort)
	switch effort {
	case "":
		// Client did not request an effort level.

	case "none":
		// OpenAI's "none" means reasoning should be disabled.
		intent.Disabled = true
		intent.Enabled = false
		intent.Effort = ""

	default:
		intent.Effort = effort
		intent.Enabled = true
	}

	return buildAdditionalModelRequestFields(intent, capability)
}

// Build only fields advertised by the model schema.
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

		if capability.SupportsThinking {
			if disabledType, ok := matchThinkingType(
				"disabled",
				capability.ThinkingTypes,
			); ok {
				return map[string]interface{}{
					"thinking": map[string]interface{}{
						"type": disabledType,
					},
				}, true, nil
			}
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

		thinkingType, ok := selectThinkingType(
			intent.ThinkingType,
			capability.ThinkingTypes,
		)

		if !ok {
			return nil, true, fmt.Errorf(
				"model %s does not support thinking.type %q; supported: %s",
				capability.ModelID,
				intent.ThinkingType,
				strings.Join(
					capability.ThinkingTypes,
					", ",
				),
			)
		}

		if thinkingType != "" {
			thinking["type"] = thinkingType
		}

		if intent.Display != "" {
			if !capability.SupportsDisplay {
				return nil, true, fmt.Errorf(
					"model %s does not support thinking.display",
					capability.ModelID,
				)
			}

			display := intent.Display

			// If the schema defines an enum, only accept values present in the enum.
			if len(capability.ThinkingDisplays) > 0 {
				var ok bool

				display, ok = matchSupportedDisplay(
					intent.Display,
					capability.ThinkingDisplays,
				)
				if !ok {
					return nil, true, fmt.Errorf(
						"model %s does not support thinking.display %q; supported: %s",
						capability.ModelID,
						intent.Display,
						strings.Join(
							capability.ThinkingDisplays,
							", ",
						),
					)
				}
			}

			// If the schema defines a field but no enum:
			// forward the original value sent by the client.
			thinking["display"] = display
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

func selectThinkingType(requested string, supported []string) (string, bool) {
	requested = strings.TrimSpace(requested)

	if requested != "" {
		for _, supportedType := range supported {
			if strings.EqualFold(
				supportedType,
				requested,
			) {
				return supportedType, true
			}
		}

		return "", false
	}

	for _, preferred := range []string{
		"adaptive",
		"enabled",
	} {
		for _, supportedType := range supported {
			if strings.EqualFold(
				supportedType,
				preferred,
			) {
				return supportedType, true
			}
		}
	}

	if len(supported) > 0 {
		return supported[0], true
	}

	return "", false
}

func matchThinkingType(
	requested string,
	supported []string,
) (string, bool) {
	requested = strings.TrimSpace(requested)

	for _, value := range supported {
		if strings.EqualFold(
			strings.TrimSpace(value),
			requested,
		) {
			return value, true
		}
	}

	return "", false
}

func normalizeClaudeThinkingType(
	requested string,
	supported []string,
) string {
	requested = strings.ToLower(
		strings.TrimSpace(requested),
	)

	switch requested {
	case "enabled":
		// Keep enabled when the model natively supports it.
		if value, ok := matchThinkingType(
			"enabled",
			supported,
		); ok {
			return value
		}

		// Claude Code's enabled maps to Kiro adaptive thinking.
		if value, ok := matchThinkingType(
			"adaptive",
			supported,
		); ok {
			return value
		}

	case "none":
		if value, ok := matchThinkingType(
			"disabled",
			supported,
		); ok {
			return value
		}
	}

	return requested
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

func matchSupportedDisplay(
	requested string,
	supported []string,
) (string, bool) {
	requested = strings.TrimSpace(requested)

	for _, value := range supported {
		if strings.EqualFold(value, requested) {
			return value, true
		}
	}

	return "", false
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
