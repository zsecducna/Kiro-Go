package proxy

import (
	"encoding/json"
	"testing"
)

// TestOpenAIToolAcceptsResponsesFlatFormat verifies that the Responses API tool
// shape (name/description/parameters at the top level) is parsed correctly, not
// just the Chat Completions nested {"function":{...}} shape. Previously the flat
// form produced an empty Function.Name, which Kiro rejected with HTTP 400
// "Improperly formed request".
func TestOpenAIToolAcceptsResponsesFlatFormat(t *testing.T) {
	flat := `{"type":"function","name":"exec_command","description":"Run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}`
	var tool OpenAITool
	if err := json.Unmarshal([]byte(flat), &tool); err != nil {
		t.Fatalf("unmarshal flat tool: %v", err)
	}
	if tool.Function.Name != "exec_command" {
		t.Fatalf("expected name exec_command, got %q", tool.Function.Name)
	}
	if tool.Function.Description != "Run a shell command" {
		t.Fatalf("expected description preserved, got %q", tool.Function.Description)
	}
	if tool.Function.Parameters == nil {
		t.Fatalf("expected parameters preserved")
	}
}

// TestOpenAIToolAcceptsNestedFormat verifies the Chat Completions nested shape
// still works after adding flat-format support.
func TestOpenAIToolAcceptsNestedFormat(t *testing.T) {
	nested := `{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object"}}}`
	var tool OpenAITool
	if err := json.Unmarshal([]byte(nested), &tool); err != nil {
		t.Fatalf("unmarshal nested tool: %v", err)
	}
	if tool.Function.Name != "get_weather" {
		t.Fatalf("expected name get_weather, got %q", tool.Function.Name)
	}
}

func TestOpenAIToolAcceptsCustomFormat(t *testing.T) {
	custom := `{
		"type":"custom",
		"name":"exec",
		"description":"Run JavaScript that orchestrates terminal tools",
		"format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}
	}`
	var tool OpenAITool
	if err := json.Unmarshal([]byte(custom), &tool); err != nil {
		t.Fatalf("unmarshal custom tool: %v", err)
	}
	if tool.Type != "custom" || tool.Function.Name != "exec" {
		t.Fatalf("unexpected custom tool: type=%q name=%q", tool.Type, tool.Function.Name)
	}
	if tool.Function.Description == "" {
		t.Fatal("expected custom tool description to be preserved")
	}
	if len(tool.Format) == 0 {
		t.Fatal("expected custom tool format to be preserved")
	}
}

// TestConvertOpenAIToolsEmitsNonEmptyNames ensures the converter never emits a
// tool spec with an empty name (Kiro rejects those) and preserves valid names.
func TestConvertOpenAIToolsEmitsNonEmptyNames(t *testing.T) {
	tools := []OpenAITool{
		mustTool(t, `{"type":"function","name":"exec_command","parameters":{"type":"object"}}`),
		mustTool(t, `{"type":"function","function":{"name":"update_plan","parameters":{"type":"object"}}}`),
	}
	wrappers := convertOpenAITools(tools)
	if len(wrappers) != 2 {
		t.Fatalf("expected 2 tool wrappers, got %d", len(wrappers))
	}
	for i, w := range wrappers {
		if w.ToolSpecification.Name == "" {
			t.Fatalf("tool %d has empty name", i)
		}
	}
	if wrappers[0].ToolSpecification.Name != "exec_command" {
		t.Fatalf("expected exec_command preserved, got %q", wrappers[0].ToolSpecification.Name)
	}
	if wrappers[1].ToolSpecification.Name != "update_plan" {
		t.Fatalf("expected update_plan preserved, got %q", wrappers[1].ToolSpecification.Name)
	}
}

func TestConvertOpenAICustomToolWrapsFreeformInput(t *testing.T) {
	tool := mustTool(t, `{"type":"custom","name":"exec","description":"Run terminal JavaScript"}`)
	wrappers := convertOpenAITools([]OpenAITool{tool})
	if len(wrappers) != 1 {
		t.Fatalf("expected one custom tool wrapper, got %d", len(wrappers))
	}
	if wrappers[0].ToolSpecification.Name != "exec" {
		t.Fatalf("expected exec tool name, got %q", wrappers[0].ToolSpecification.Name)
	}
	schema, ok := wrappers[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok {
		t.Fatalf("expected object schema, got %#v", wrappers[0].ToolSpecification.InputSchema.JSON)
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok || properties[customToolInputField] == nil {
		t.Fatalf("expected %q string wrapper property, schema=%#v", customToolInputField, schema)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != customToolInputField {
		t.Fatalf("expected required input field, schema=%#v", schema)
	}
}

func mustTool(t *testing.T, s string) OpenAITool {
	t.Helper()
	var tool OpenAITool
	if err := json.Unmarshal([]byte(s), &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return tool
}
