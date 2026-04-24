package httpapi

import (
	"encoding/json"
	"testing"
)

func TestNormalizeAnthropicRequestPropagatesCacheControl(t *testing.T) {
	raw := `{
		"model": "claude-sonnet",
		"system": [
			{"type": "text", "text": "stable instructions"},
			{"type": "text", "text": "large context", "cache_control": {"type": "ephemeral"}},
			{"type": "text", "text": "tail"}
		],
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "prefix"},
					{"type": "text", "text": "cache here", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "question"}
				]
			}
		],
		"tools": [
			{"name": "a", "input_schema": {"type": "object", "properties": {}}},
			{"name": "b", "input_schema": {"type": "object", "properties": {}}, "cache_control": {"type": "ephemeral"}}
		]
	}`
	var req AnthropicMessagesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	normalized, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	if len(normalized.SystemCacheAfter) != 1 || normalized.SystemCacheAfter[0] != 1 {
		t.Fatalf("SystemCacheAfter = %#v, want [1]", normalized.SystemCacheAfter)
	}

	if len(normalized.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(normalized.Messages))
	}
	blocks := normalized.Messages[0].Blocks
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if blocks[0].CacheHint || !blocks[1].CacheHint || blocks[2].CacheHint {
		t.Fatalf("block CacheHint = [%v %v %v], want [false true false]",
			blocks[0].CacheHint, blocks[1].CacheHint, blocks[2].CacheHint)
	}

	if len(normalized.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(normalized.Tools))
	}
	if normalized.Tools[0].CacheHint || !normalized.Tools[1].CacheHint {
		t.Fatalf("tool CacheHint = [%v %v], want [false true]",
			normalized.Tools[0].CacheHint, normalized.Tools[1].CacheHint)
	}
}

func TestNormalizeAnthropicRequestWithoutCacheControl(t *testing.T) {
	raw := `{
		"model": "claude-sonnet",
		"system": "one big system prompt",
		"messages": [{"role": "user", "content": "hi"}]
	}`
	var req AnthropicMessagesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	normalized, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(normalized.SystemCacheAfter) != 0 {
		t.Fatalf("SystemCacheAfter = %#v, want empty", normalized.SystemCacheAfter)
	}
	for _, msg := range normalized.Messages {
		for _, block := range msg.Blocks {
			if block.CacheHint {
				t.Fatalf("unexpected CacheHint on block %#v", block)
			}
		}
	}
}
