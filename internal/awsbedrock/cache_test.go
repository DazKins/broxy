package awsbedrock

import (
	"encoding/json"
	"testing"

	"github.com/personal/broxy/internal/domain"
)

func TestBuildConversePayloadEmitsCachePointBlocks(t *testing.T) {
	req := domain.ConverseRequest{
		ModelID: "anthropic.claude-sonnet",
		Region:  "us-east-1",
		System: []string{
			"stable",
			"cached-prefix",
			"tail",
		},
		SystemCacheAfter: []int{1},
		Messages: []domain.BedrockChatMessage{{
			Role: "user",
			Blocks: []domain.BedrockContentBlock{
				{Type: "text", Text: "prefix"},
				{Type: "text", Text: "cache here", CacheHint: true},
				{Type: "text", Text: "question"},
			},
		}},
		Tools: []domain.ToolDefinition{
			{Name: "a", Parameters: []byte(`{"type":"object","properties":{}}`)},
			{Name: "b", Parameters: []byte(`{"type":"object","properties":{}}`), CacheHint: true},
		},
		ToolChoice: &domain.ToolChoice{Type: "auto"},
	}

	payload, err := buildConversePayload(req)
	if err != nil {
		t.Fatalf("buildConversePayload: %v", err)
	}

	system, ok := payload["system"].([]map[string]any)
	if !ok {
		t.Fatalf("system type = %T", payload["system"])
	}
	if len(system) != 4 {
		t.Fatalf("system len = %d, want 4 (3 texts + 1 cache point)", len(system))
	}
	if _, ok := system[0]["text"]; !ok {
		t.Fatalf("system[0] missing text: %#v", system[0])
	}
	if _, ok := system[1]["text"]; !ok {
		t.Fatalf("system[1] missing text: %#v", system[1])
	}
	cacheSys, ok := system[2]["cachePoint"].(map[string]any)
	if !ok {
		t.Fatalf("system[2] missing cachePoint: %#v", system[2])
	}
	if cacheSys["type"] != "default" {
		t.Fatalf("system cache point type = %#v", cacheSys["type"])
	}
	if _, ok := system[3]["text"]; !ok {
		t.Fatalf("system[3] missing text: %#v", system[3])
	}

	messages, ok := payload["messages"].([]map[string]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	content, ok := messages[0]["content"].([]map[string]any)
	if !ok {
		t.Fatalf("content type = %T", messages[0]["content"])
	}
	if len(content) != 4 {
		t.Fatalf("content len = %d, want 4", len(content))
	}
	cacheMsg, ok := content[2]["cachePoint"].(map[string]any)
	if !ok {
		t.Fatalf("content[2] missing cachePoint: %#v", content[2])
	}
	if cacheMsg["type"] != "default" {
		t.Fatalf("content cache point type = %#v", cacheMsg["type"])
	}

	toolConfig, ok := payload["toolConfig"].(map[string]any)
	if !ok {
		t.Fatalf("toolConfig missing: %#v", payload)
	}
	tools, ok := toolConfig["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools type = %T", toolConfig["tools"])
	}
	if len(tools) != 3 {
		t.Fatalf("tools len = %d, want 3 (2 specs + 1 cache point)", len(tools))
	}
	if _, ok := tools[0]["toolSpec"]; !ok {
		t.Fatalf("tools[0] missing toolSpec: %#v", tools[0])
	}
	if _, ok := tools[1]["toolSpec"]; !ok {
		t.Fatalf("tools[1] missing toolSpec: %#v", tools[1])
	}
	if _, ok := tools[2]["cachePoint"]; !ok {
		t.Fatalf("tools[2] missing cachePoint: %#v", tools[2])
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("payload not valid JSON: %s", raw)
	}
}

func TestBuildConversePayloadNoCacheHintsEmitsNoCachePoints(t *testing.T) {
	req := domain.ConverseRequest{
		ModelID: "anthropic.claude-sonnet",
		Region:  "us-east-1",
		System:  []string{"only"},
		Messages: []domain.BedrockChatMessage{{
			Role: "user",
			Blocks: []domain.BedrockContentBlock{
				{Type: "text", Text: "plain"},
			},
		}},
		Tools: []domain.ToolDefinition{
			{Name: "a", Parameters: []byte(`{"type":"object","properties":{}}`)},
		},
	}
	payload, err := buildConversePayload(req)
	if err != nil {
		t.Fatalf("buildConversePayload: %v", err)
	}
	raw, _ := json.Marshal(payload)
	if string(raw) == "" {
		t.Fatalf("empty payload")
	}
	if containsCachePoint(raw) {
		t.Fatalf("unexpected cachePoint in payload: %s", raw)
	}
}

func containsCachePoint(raw []byte) bool {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false
	}
	return searchCachePoint(parsed)
}

func searchCachePoint(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		if _, ok := val["cachePoint"]; ok {
			return true
		}
		for _, child := range val {
			if searchCachePoint(child) {
				return true
			}
		}
	case []any:
		for _, child := range val {
			if searchCachePoint(child) {
				return true
			}
		}
	case []map[string]any:
		for _, child := range val {
			if searchCachePoint(child) {
				return true
			}
		}
	}
	return false
}
