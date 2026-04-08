package awsbedrock

import (
	"encoding/json"
	"testing"

	"github.com/personal/broxy/internal/domain"
)

func TestBuildConversePayloadIncludesExecCommandSchema(t *testing.T) {
	req := domain.ConverseRequest{
		ModelID: "global.anthropic.claude-opus-4-6-v1",
		Region:  "us-east-1",
		System:  []string{"system prompt"},
		Messages: []domain.BedrockChatMessage{{
			Role:    "user",
			Content: "Read README.md",
		}},
		Tools: []domain.ToolDefinition{{
			Name:        "exec_command",
			Description: "Runs a shell command.",
			Parameters:  []byte(`{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}`),
		}},
		ToolChoice: &domain.ToolChoice{Type: "auto"},
	}

	payload, err := buildConversePayload(req)
	if err != nil {
		t.Fatalf("buildConversePayload() error = %v", err)
	}

	toolConfig, ok := payload["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("toolConfig missing from payload: %#v", payload)
	}
	tools, ok := toolConfig["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("unexpected tools payload: %#v", toolConfig["tools"])
	}
	toolSpec, ok := tools[0]["toolSpec"].(map[string]any)
	if !ok {
		t.Fatalf("toolSpec missing: %#v", tools[0])
	}
	if toolSpec["name"] != "exec_command" {
		t.Fatalf("tool name = %#v", toolSpec["name"])
	}
	inputSchema, ok := toolSpec["inputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("inputSchema missing: %#v", toolSpec)
	}
	schemaJSON, err := json.Marshal(inputSchema["json"])
	if err != nil {
		t.Fatalf("json.Marshal(schema) error = %v", err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatalf("json.Unmarshal(schema) error = %v", err)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "cmd" {
		t.Fatalf("required = %#v", schema.Required)
	}
	if _, ok := schema.Properties["cmd"]; !ok {
		t.Fatalf("cmd property missing from schema: %s", schemaJSON)
	}
	if toolChoice, ok := toolConfig["toolChoice"].(map[string]any); !ok || toolChoice["auto"] == nil {
		t.Fatalf("toolChoice missing auto: %#v", toolConfig["toolChoice"])
	}
}
