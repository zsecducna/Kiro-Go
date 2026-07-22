package proxy

import (
	"encoding/json"
	"testing"
)

func TestParseOutputConfigReasoningSchema(t *testing.T) {
	model := ModelInfo{
		ModelId: "test-model",
		AdditionalModelRequestFieldsSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"thinking": {
					"type": "object",
					"properties": {
						"type": {
							"enum": ["adaptive"]
						},
						"display": {
							"enum": ["summarized", "omitted"]
						}
					}
				},
				"output_config": {
					"type": "object",
					"properties": {
						"effort": {
							"enum": ["low", "medium", "high", "xhigh", "max"]
						}
					}
				}
			}
		}`),
	}

	capability :=
		ParseModelReasoningCapability(model)

	if !capability.SupportsThinking {
		t.Fatal("expected thinking support")
	}
	if capability.EffortPath != ReasoningSchemaOutputConfig {
		t.Fatalf(
			"unexpected effort path: %s",
			capability.EffortPath,
		)
	}
	if len(capability.Efforts) != 5 {
		t.Fatalf(
			"unexpected efforts: %#v",
			capability.Efforts,
		)
	}
	if len(capability.ThinkingDisplays) != 2 {
		t.Fatalf(
			"unexpected thinking displays: %#v",
			capability.ThinkingDisplays,
		)
	}
}

func TestRejectUnsupportedThinkingDisplay(
	t *testing.T,
) {
	req := &ClaudeRequest{
		Model: "test-model",
		Thinking: &ClaudeThinkingConfig{
			Type:    "adaptive",
			Display: "full",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
		SupportsDisplay:  true,
		ThinkingDisplays: []string{
			"summarized",
			"omitted",
		},
	}

	_, _, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err == nil {
		t.Fatal(
			"expected unsupported display error",
		)
	}
}

func TestRejectUnsupportedThinkingType(
	t *testing.T,
) {
	req := &ClaudeRequest{
		Model: "test-model",
		Thinking: &ClaudeThinkingConfig{
			Type: "forced",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes: []string{
			"adaptive",
			"disabled",
		},
	}

	_, requested, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err == nil {
		t.Fatal("expected unsupported thinking type error")
	}

	if !requested {
		t.Fatal("request should be recognized as a reasoning request")
	}
}

func TestClaudeEnabledThinkingMapsToAdaptive(
	t *testing.T,
) {
	req := &ClaudeRequest{
		Model: "claude-opus-4.8",
		Thinking: &ClaudeThinkingConfig{
			Type:         "enabled",
			BudgetTokens: 10000,
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "claude-opus-4.8",
		SupportsThinking: true,
		ThinkingTypes: []string{
			"adaptive",
			"disabled",
		},
	}

	fields, requested, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !requested {
		t.Fatal("expected reasoning to be requested")
	}

	thinking, ok := fields["thinking"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing thinking fields: %#v", fields)
	}

	if thinking["type"] != "adaptive" {
		t.Fatalf("thinking.type = %#v, want adaptive", thinking["type"])
	}
}

func TestParseReasoningPathSchema(t *testing.T) {
	model := ModelInfo{
		ModelId: "test-model",
		AdditionalModelRequestFieldsSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"reasoning": {
					"type": "object",
					"properties": {
						"effort": {
							"enum": ["low", "medium", "high"]
						}
					}
				}
			}
		}`),
	}

	capability :=
		ParseModelReasoningCapability(model)

	if capability.EffortPath != ReasoningSchemaReasoning {
		t.Fatalf(
			"unexpected effort path: %s",
			capability.EffortPath,
		)
	}
}

func TestBuildClaudeNativeFields(t *testing.T) {
	req := &ClaudeRequest{
		Model:     "test-model",
		MaxTokens: 16000,
		Thinking: &ClaudeThinkingConfig{
			Type:    "adaptive",
			Display: "summarized",
		},
		OutputConfig: &ClaudeOutputConfig{
			Effort: "xhigh",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
		SupportsDisplay:  true,
		EffortPath:       ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
			"xhigh",
			"max",
		},
	}

	fields, requested, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatal(err)
	}
	if !requested {
		t.Fatal("expected reasoning request")
	}

	outputConfig, ok :=
		fields["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf(
			"missing output_config: %#v",
			fields,
		)
	}

	if outputConfig["effort"] != "xhigh" {
		t.Fatalf(
			"unexpected effort: %#v",
			outputConfig,
		)
	}
}

func TestRejectUnsupportedEffort(t *testing.T) {
	req := &ClaudeRequest{
		Model: "test-model",
		Thinking: &ClaudeThinkingConfig{
			Type: "adaptive",
		},
		OutputConfig: &ClaudeOutputConfig{
			Effort: "max",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
		EffortPath:       ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
		},
	}

	_, _, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err == nil {
		t.Fatal("expected unsupported effort error")
	}
}

func TestOpenAIEnabledThinkingMapsToAdaptive(t *testing.T) {
	req := &OpenAIRequest{
		Model: "claude-opus-4.8",
		Thinking: &ClaudeThinkingConfig{
			Type: "enabled",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "claude-opus-4.8",
		SupportsThinking: true,
		ThinkingTypes: []string{
			"adaptive",
			"disabled",
		},
	}

	fields, requested, err :=
		BuildOpenAIAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !requested {
		t.Fatal("expected reasoning to be requested")
	}

	thinking := requireReasoningMap(
		t,
		fields,
		"thinking",
	)

	if got := thinking["type"]; got != "adaptive" {
		t.Fatalf(
			"thinking.type = %#v, want adaptive",
			got,
		)
	}
}

func TestBudgetTokensForwardedWhenSchemaSupportsIt(
	t *testing.T,
) {
	req := &ClaudeRequest{
		Model: "test-model",
		Thinking: &ClaudeThinkingConfig{
			Type:         "adaptive",
			BudgetTokens: 8192,
		},
	}

	capability := ModelReasoningCapability{
		ModelID:              "test-model",
		SupportsThinking:     true,
		ThinkingTypes:        []string{"adaptive"},
		SupportsBudgetTokens: true,
	}

	fields, requested, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !requested {
		t.Fatal("expected reasoning to be requested")
	}

	thinking := requireReasoningMap(
		t,
		fields,
		"thinking",
	)

	if got := thinking["budget_tokens"]; got != 8192 {
		t.Fatalf(
			"thinking.budget_tokens = %#v, want 8192",
			got,
		)
	}
}

func TestBudgetTokensOmittedWhenSchemaDoesNotSupportIt(
	t *testing.T,
) {
	req := &ClaudeRequest{
		Model: "test-model",
		Thinking: &ClaudeThinkingConfig{
			Type:         "adaptive",
			BudgetTokens: 8192,
		},
	}

	capability := ModelReasoningCapability{
		ModelID:              "test-model",
		SupportsThinking:     true,
		ThinkingTypes:        []string{"adaptive"},
		SupportsBudgetTokens: false,
	}

	fields, requested, err := BuildClaudeAdditionalModelRequestFields(req, capability)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !requested {
		t.Fatal("expected reasoning to be requested")
	}

	thinking := requireReasoningMap(
		t,
		fields,
		"thinking",
	)

	if _, exists := thinking["budget_tokens"]; exists {
		t.Fatalf(
			"budget_tokens should be omitted: %#v",
			thinking,
		)
	}

	if got := thinking["type"]; got != "adaptive" {
		t.Fatalf(
			"thinking.type = %#v, want adaptive",
			got,
		)
	}
}

func TestExplicitEffortAndBudgetTokensFollowSchemaIndependently(
	t *testing.T,
) {
	req := &OpenAIRequest{
		Model: "test-model",
		Thinking: &ClaudeThinkingConfig{
			Type:         "adaptive",
			BudgetTokens: 4096,
		},
		OutputConfig: &ClaudeOutputConfig{
			Effort: "high",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:              "test-model",
		SupportsThinking:     true,
		ThinkingTypes:        []string{"adaptive"},
		SupportsBudgetTokens: true,
		EffortPath:           ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
		},
	}

	fields, requested, err :=
		BuildOpenAIAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !requested {
		t.Fatal("expected reasoning to be requested")
	}

	thinking := requireReasoningMap(
		t,
		fields,
		"thinking",
	)

	if got := thinking["budget_tokens"]; got != 4096 {
		t.Fatalf(
			"thinking.budget_tokens = %#v, want 4096",
			got,
		)
	}

	outputConfig := requireReasoningMap(
		t,
		fields,
		"output_config",
	)

	if got := outputConfig["effort"]; got != "high" {
		t.Fatalf(
			"output_config.effort = %#v, want high",
			got,
		)
	}
}

func TestOpenAIReasoningEffortPrecedence(t *testing.T) {
	tests := []struct {
		name string
		req  *OpenAIRequest
		want string
	}{
		{
			name: "output_config takes precedence",
			req: &OpenAIRequest{
				OutputConfig: &ClaudeOutputConfig{
					Effort: "high",
				},
				Reasoning: &OpenAIReasoningConfig{
					Effort: "medium",
				},
				ReasoningEffort: "low",
			},
			want: "high",
		},
		{
			name: "reasoning object takes precedence over legacy field",
			req: &OpenAIRequest{
				Reasoning: &OpenAIReasoningConfig{
					Effort: "medium",
				},
				ReasoningEffort: "low",
			},
			want: "medium",
		},
		{
			name: "legacy reasoning_effort remains supported",
			req: &OpenAIRequest{
				ReasoningEffort: "low",
			},
			want: "low",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
		EffortPath:       ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields, requested, err :=
				BuildOpenAIAdditionalModelRequestFields(
					test.req,
					capability,
				)

			if err != nil {
				t.Fatalf(
					"unexpected error: %v",
					err,
				)
			}

			if !requested {
				t.Fatal(
					"expected reasoning to be requested",
				)
			}

			outputConfig := requireReasoningMap(
				t,
				fields,
				"output_config",
			)

			if got := outputConfig["effort"]; got != test.want {

				t.Fatalf(
					"effort = %#v, want %q",
					got,
					test.want,
				)
			}
		})
	}
}

func TestOpenAIReasoningEffortNoneDisablesReasoning(
	t *testing.T,
) {
	req := &OpenAIRequest{
		Model: "test-model",
		Reasoning: &OpenAIReasoningConfig{
			Effort: "none",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes: []string{
			"adaptive",
			"disabled",
		},
		EffortPath: ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
		},
	}

	fields, requested, err :=
		BuildOpenAIAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requested {
		t.Fatal(
			"reasoning should not be requested for effort=none",
		)
	}

	if len(fields) != 0 {
		t.Fatalf(
			"expected no native reasoning fields, got %#v",
			fields,
		)
	}
}

func TestUnsupportedOpenAIEffortIsRejected(
	t *testing.T,
) {
	req := &OpenAIRequest{
		Model: "test-model",
		Reasoning: &OpenAIReasoningConfig{
			Effort: "minimal",
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
		EffortPath:       ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
		},
	}

	_, requested, err :=
		BuildOpenAIAdditionalModelRequestFields(
			req,
			capability,
		)

	if err == nil {
		t.Fatal(
			"expected unsupported effort error",
		)
	}

	if !requested {
		t.Fatal(
			"request should be recognized as a reasoning request",
		)
	}
}

func TestOpenAINoReasoningFieldsKeepsDefaultBehavior(
	t *testing.T,
) {
	req := &OpenAIRequest{
		Model: "test-model",
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
		EffortPath:       ReasoningSchemaOutputConfig,
		Efforts: []string{
			"low",
			"medium",
			"high",
		},
	}

	fields, requested, err :=
		BuildOpenAIAdditionalModelRequestFields(
			req,
			capability,
		)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if requested {
		t.Fatal(
			"reasoning should not be requested",
		)
	}

	if len(fields) != 0 {
		t.Fatalf(
			"expected no reasoning fields, got %#v",
			fields,
		)
	}
}

func requireReasoningMap(
	t *testing.T,
	fields map[string]interface{},
	key string,
) map[string]interface{} {
	t.Helper()

	if fields == nil {
		t.Fatalf(
			"fields is nil; expected %q",
			key,
		)
	}

	value, exists := fields[key]
	if !exists {
		t.Fatalf(
			"missing %q in fields: %#v",
			key,
			fields,
		)
	}

	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf(
			"%q has type %T, want map[string]interface{}",
			key,
			value,
		)
	}

	return result
}
