package awsbedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	brdocument "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/logging"
)

func TestLogAuthLogsAWSEnvAuth(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIATEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")

	var logs bytes.Buffer
	client, err := NewWithLogger(context.Background(), config.UpstreamConfig{
		Mode:   config.UpstreamAuthAWS,
		Region: "us-east-1",
	}, logging.New("info", &logs))
	if err != nil {
		t.Fatalf("NewWithLogger() error = %v", err)
	}

	client.LogAuth(context.Background())

	logText := logs.String()
	if !strings.Contains(logText, "bedrock auth configured") {
		t.Fatalf("missing auth log: %s", logText)
	}
	if !strings.Contains(logText, `auth_method="AWS access keys from environment"`) {
		t.Fatalf("missing auth method: %s", logText)
	}
	if !strings.Contains(logText, "sdk_source=EnvConfigCredentials") {
		t.Fatalf("missing SDK credential source: %s", logText)
	}
}

func TestLogAuthLogsBearerAuthWithoutTokenValue(t *testing.T) {
	const token = "secret-bedrock-api-key"

	var logs bytes.Buffer
	client, err := NewWithLogger(context.Background(), config.UpstreamConfig{
		Mode:        config.UpstreamAuthBearer,
		Region:      "us-east-1",
		BearerToken: token,
	}, logging.New("info", &logs))
	if err != nil {
		t.Fatalf("NewWithLogger() error = %v", err)
	}

	client.LogAuth(context.Background())

	logText := logs.String()
	if !strings.Contains(logText, "bedrock auth configured") {
		t.Fatalf("missing auth log: %s", logText)
	}
	if !strings.Contains(logText, `auth_method="Bedrock API key"`) {
		t.Fatalf("missing bearer auth method: %s", logText)
	}
	if !strings.Contains(logText, "token_configured=true") {
		t.Fatalf("missing token configured flag: %s", logText)
	}
	if strings.Contains(logText, token) {
		t.Fatalf("log leaked bearer token: %s", logText)
	}
}

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

func TestFromSDKContentBlockMarshalsToolInputDocument(t *testing.T) {
	block, err := fromSDKContentBlock(&brtypes.ContentBlockMemberToolUse{
		Value: brtypes.ToolUseBlock{
			ToolUseId: ptrString("tooluse_1"),
			Name:      ptrString("exec_command"),
			Input: brdocument.NewLazyDocument(map[string]any{
				"cmd": "pwd",
			}),
		},
	})
	if err != nil {
		t.Fatalf("fromSDKContentBlock() error = %v", err)
	}
	if block.Type != "tool_use" {
		t.Fatalf("type = %q", block.Type)
	}
	var input struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal(block.ToolInput, &input); err != nil {
		t.Fatalf("json.Unmarshal(tool input) error = %v; input=%s", err, block.ToolInput)
	}
	if input.Cmd != "pwd" {
		t.Fatalf("cmd = %q, input=%s", input.Cmd, block.ToolInput)
	}
}

func TestJsonToolConfigAddsMissingAdditionalPropertiesFalse(t *testing.T) {
	toolConfig, err := jsonToolConfig([]domain.ToolDefinition{{
		Name: "mcp__codex_apps__github_add_comment_to_issue",
		Parameters: []byte(`{
			"type": "object",
			"properties": {
				"comment": {"type": "string"},
				"metadata": {
					"type": "object",
					"properties": {
						"source": {"type": "string"}
					}
				}
			},
			"required": ["comment"]
		}`),
	}}, nil)
	if err != nil {
		t.Fatalf("jsonToolConfig() error = %v", err)
	}
	schema := toolConfig["tools"].([]map[string]any)[0]["toolSpec"].(map[string]any)["inputSchema"].(map[string]any)["json"].(map[string]any)
	if schema["additionalProperties"] != false {
		t.Fatalf("top-level additionalProperties = %#v", schema["additionalProperties"])
	}
	metadata := schema["properties"].(map[string]any)["metadata"].(map[string]any)
	if metadata["additionalProperties"] != false {
		t.Fatalf("nested additionalProperties = %#v", metadata["additionalProperties"])
	}
}

func TestNormalizeToolSchemaPreservesExplicitAdditionalProperties(t *testing.T) {
	schema, err := decodeToolSchemaValue([]byte(`{
		"type": "object",
		"additionalProperties": true,
		"properties": {
			"closed": {
				"type": "object",
				"additionalProperties": false,
				"properties": {
					"name": {"type": "string"}
				}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("decodeToolSchemaValue() error = %v", err)
	}
	root := schema.(map[string]any)
	if root["additionalProperties"] != true {
		t.Fatalf("root additionalProperties = %#v", root["additionalProperties"])
	}
	closed := root["properties"].(map[string]any)["closed"].(map[string]any)
	if closed["additionalProperties"] != false {
		t.Fatalf("closed additionalProperties = %#v", closed["additionalProperties"])
	}
}
