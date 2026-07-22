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
			Type:         "enabled",
			BudgetTokens: 4096,
		},
	}

	capability := ModelReasoningCapability{
		ModelID:          "test-model",
		SupportsThinking: true,
		ThinkingTypes:    []string{"adaptive"},
	}

	_, _, err :=
		BuildClaudeAdditionalModelRequestFields(
			req,
			capability,
		)

	if err == nil {
		t.Fatal(
			"expected unsupported thinking type error",
		)
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
