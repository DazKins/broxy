package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/db"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/logging"
	"github.com/personal/broxy/internal/security"
)

type fakeProvider struct{}

func (fakeProvider) Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	return &domain.ConverseResponse{
		ModelID:    req.ModelID,
		Text:       "hello from bedrock",
		StopReason: "end_turn",
		Usage: domain.TokenUsage{
			Input:  12,
			Output: 8,
			Total:  20,
		},
		LatencyMS: 42,
	}, nil
}

type recordingProvider struct {
	mu       sync.Mutex
	requests []domain.ConverseRequest
}

func (p *recordingProvider) Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return fakeProvider{}.Converse(ctx, req)
}

func (p *recordingProvider) Requests() []domain.ConverseRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	items := make([]domain.ConverseRequest, len(p.requests))
	copy(items, p.requests)
	return items
}

type toolProvider struct {
	mu       sync.Mutex
	requests []domain.ConverseRequest
}

type errorProvider struct {
	err error
}

func (p errorProvider) Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	return nil, p.err
}

type upstreamStatusError struct {
	statusCode int
	message    string
}

func (e upstreamStatusError) Error() string {
	return e.message
}

func (e upstreamStatusError) HTTPStatusCode() int {
	return e.statusCode
}

func (p *toolProvider) Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()

	if hasToolResult(req.Messages) {
		return &domain.ConverseResponse{
			ModelID: req.ModelID,
			Text:    "tool completed",
			Message: domain.BedrockChatMessage{
				Role: "assistant",
				Blocks: []domain.BedrockContentBlock{{
					Type: "text",
					Text: "tool completed",
				}},
			},
			StopReason: "end_turn",
			Usage: domain.TokenUsage{
				Input:  20,
				Output: 4,
				Total:  24,
			},
			LatencyMS:   42,
			RawResponse: `{"output":{"message":{"content":[{"text":"tool completed"}]}},"stopReason":"end_turn"}`,
		}, nil
	}

	return &domain.ConverseResponse{
		ModelID: req.ModelID,
		Message: domain.BedrockChatMessage{
			Role: "assistant",
			Blocks: []domain.BedrockContentBlock{{
				Type:      "tool_use",
				ToolUseID: "call_exec_1",
				ToolName:  "exec_command",
				ToolInput: []byte(`{"cmd":"pwd"}`),
			}},
		},
		StopReason: "tool_use",
		Usage: domain.TokenUsage{
			Input:  12,
			Output: 8,
			Total:  20,
		},
		LatencyMS:   42,
		RawResponse: `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"call_exec_1","name":"exec_command","input":{"cmd":"pwd"}}}]}},"stopReason":"tool_use"}`,
	}, nil
}

func (p *toolProvider) Requests() []domain.ConverseRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	items := make([]domain.ConverseRequest, len(p.requests))
	copy(items, p.requests)
	return items
}

func hasToolResult(messages []domain.BedrockChatMessage) bool {
	for _, msg := range messages {
		for _, block := range msg.Blocks {
			if block.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

func addTestModelRoute(t *testing.T, store *db.Store) {
	t.Helper()
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}
}

func TestChatCompletionProxy(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	key, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if resp.Model != "claude-haiku-4-5" {
		t.Fatalf("response model = %q", resp.Model)
	}
	if resp.Usage.TotalTokens != 20 {
		t.Fatalf("total tokens = %d", resp.Usage.TotalTokens)
	}

	logs, err := store.ListRequestLogs(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRequestLogs() error = %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs length = %d", len(logs))
	}
	if logs[0].APIKeyID != key.ID {
		t.Fatalf("API key ID = %q, want %q", logs[0].APIKeyID, key.ID)
	}
}

func TestResponsesProxy(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")
	body := map[string]any{
		"model":        "claude-haiku-4-5",
		"instructions": "You are helpful.",
		"input": []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "hello"},
				},
			},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if resp.Object != "response" {
		t.Fatalf("object = %q", resp.Object)
	}
	if resp.Model != "claude-haiku-4-5" {
		t.Fatalf("response model = %q", resp.Model)
	}
	if len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 {
		t.Fatalf("unexpected output shape: %s", rec.Body.String())
	}
	if resp.Output[0].Content[0].Text != "hello from bedrock" {
		t.Fatalf("output text = %q", resp.Output[0].Content[0].Text)
	}
	if resp.Usage.TotalTokens != 20 {
		t.Fatalf("total tokens = %d", resp.Usage.TotalTokens)
	}
}

func TestResponsesProxyPreviousResponseID(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	provider := &recordingProvider{}
	server := New(cfg, store, provider, "test")

	firstBody := map[string]any{
		"model": "claude-haiku-4-5",
		"input": "hello",
	}
	payload, _ := json.Marshal(firstBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", rec.Code, rec.Body.String())
	}
	var firstResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("json.Unmarshal() first error = %v", err)
	}

	secondBody := map[string]any{
		"model":                "claude-haiku-4-5",
		"previous_response_id": firstResp.ID,
		"input":                "follow up",
	}
	secondPayload, _ := json.Marshal(secondBody)
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(secondPayload))
	secondReq.Header.Set("Authorization", "Bearer bpx_test_secret")
	secondRec := httptest.NewRecorder()
	server.Router().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", secondRec.Code, secondRec.Body.String())
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[1].Messages) != 3 {
		t.Fatalf("second request messages = %#v", requests[1].Messages)
	}
	if requests[1].Messages[0].Content != "hello" || requests[1].Messages[1].Content != "hello from bedrock" || requests[1].Messages[2].Content != "follow up" {
		t.Fatalf("unexpected chained messages = %#v", requests[1].Messages)
	}
}

func TestResponsesProxyStreaming(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")
	body := map[string]any{
		"model":  "claude-haiku-4-5",
		"input":  "hello",
		"stream": true,
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, "\"type\":\"response.output_text.delta\"") {
		t.Fatalf("missing output_text delta event: %s", bodyText)
	}
	if !strings.Contains(bodyText, "\"type\":\"response.completed\"") {
		t.Fatalf("missing response completed event: %s", bodyText)
	}
	if !strings.Contains(bodyText, "data: [DONE]") {
		t.Fatalf("missing DONE marker: %s", bodyText)
	}
}

func TestResponsesProxyToolCall(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	addTestModelRoute(t, store)
	provider := &toolProvider{}
	server := New(cfg, store, provider, "test")

	body := map[string]any{
		"model": "claude-haiku-4-5",
		"input": "run pwd",
		"tools": []map[string]any{{
			"type":        "function",
			"name":        "exec_command",
			"description": "Run a command",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []string{"cmd"},
			},
		}},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID     string `json:"id"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			CallID    string `json:"call_id"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" {
		t.Fatalf("unexpected output: %s", rec.Body.String())
	}
	if resp.Output[0].Name != "exec_command" || resp.Output[0].CallID != "call_exec_1" || resp.Output[0].Arguments != "{\"cmd\":\"pwd\"}" {
		t.Fatalf("unexpected function call item: %#v", resp.Output[0])
	}

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "exec_command" {
		t.Fatalf("unexpected tools = %#v", requests[0].Tools)
	}
	if requests[0].ToolChoice == nil || requests[0].ToolChoice.Type != "auto" {
		t.Fatalf("unexpected default tool choice = %#v", requests[0].ToolChoice)
	}

	secondBody := map[string]any{
		"model":                "claude-haiku-4-5",
		"previous_response_id": resp.ID,
		"input": []map[string]any{{
			"type":    "function_call_output",
			"call_id": "call_exec_1",
			"output":  "/Users/personal/broxy",
		}},
	}
	secondPayload, _ := json.Marshal(secondBody)
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(secondPayload))
	secondReq.Header.Set("Authorization", "Bearer bpx_test_secret")
	secondRec := httptest.NewRecorder()
	server.Router().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status = %d body=%s", secondRec.Code, secondRec.Body.String())
	}
	var secondResp struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(secondRec.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("json.Unmarshal() second error = %v", err)
	}
	if len(secondResp.Output) != 1 || len(secondResp.Output[0].Content) != 1 || secondResp.Output[0].Content[0].Text != "tool completed" {
		t.Fatalf("unexpected second response: %s", secondRec.Body.String())
	}

	requests = provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[1].Messages) != 3 {
		t.Fatalf("unexpected chained messages = %#v", requests[1].Messages)
	}
	if requests[1].Messages[1].Blocks[0].Type != "tool_use" || requests[1].Messages[2].Blocks[0].Type != "tool_result" {
		t.Fatalf("unexpected tool messages = %#v", requests[1].Messages)
	}
}

func TestResponsesProxyToolCallJSONOutput(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	addTestModelRoute(t, store)
	provider := &toolProvider{}
	server := New(cfg, store, provider, "test")

	initial := map[string]any{
		"model": "claude-haiku-4-5",
		"input": "run pwd",
		"tools": []map[string]any{{
			"type": "function",
			"name": "exec_command",
			"parameters": map[string]any{
				"type": "object",
			},
		}},
	}
	payload, _ := json.Marshal(initial)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var initialResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &initialResp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	followUp := map[string]any{
		"model":                "claude-haiku-4-5",
		"previous_response_id": initialResp.ID,
		"input": []map[string]any{{
			"type":    "function_call_output",
			"call_id": "call_exec_1",
			"output": map[string]any{
				"cwd": "/Users/personal/broxy",
			},
		}},
	}
	followPayload, _ := json.Marshal(followUp)
	followReq := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(followPayload))
	followReq.Header.Set("Authorization", "Bearer bpx_test_secret")
	followRec := httptest.NewRecorder()
	server.Router().ServeHTTP(followRec, followReq)
	if followRec.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d body=%s", followRec.Code, followRec.Body.String())
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d", len(requests))
	}
	toolResult := requests[1].Messages[2].Blocks[0]
	if toolResult.Type != "tool_result" || len(toolResult.ToolResult) != 1 || toolResult.ToolResult[0].Type != "json" || string(toolResult.ToolResult[0].JSON) != "{\"cwd\":\"/Users/personal/broxy\"}" {
		t.Fatalf("unexpected tool result payload = %#v", toolResult)
	}
}

func TestResponsesProxyMovesUserTextAfterToolResult(t *testing.T) {
	input := []map[string]any{
		{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "Update the code",
			}},
		},
		{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": "I'll patch it.",
			}},
		},
		{
			"type":      "function_call",
			"name":      "exec_command",
			"call_id":   "tooluse_1",
			"arguments": `{"cmd":"apply_patch"}`,
		},
		{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": "Warning: apply_patch was requested via exec_command. Use the apply_patch tool instead of exec_command.",
			}},
		},
		{
			"type":    "function_call_output",
			"call_id": "tooluse_1",
			"output":  "Success. Updated the files.",
		},
	}
	payload, _ := json.Marshal(input)
	normalized, err := normalizeResponseRequest(ResponseRequest{
		Model: "claude-haiku-4-5",
		Input: payload,
	}, nil)
	if err != nil {
		t.Fatalf("normalizeResponseRequest() error = %v", err)
	}
	if len(normalized.Messages) != 3 {
		t.Fatalf("unexpected messages = %#v", normalized.Messages)
	}
	blocks := normalized.Messages[2].Blocks
	if len(blocks) != 2 {
		t.Fatalf("unexpected user blocks = %#v", blocks)
	}
	if blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "tooluse_1" {
		t.Fatalf("tool result was not first: %#v", blocks)
	}
	if blocks[1].Type != "text" || !strings.Contains(blocks[1].Text, "apply_patch was requested") {
		t.Fatalf("warning text was not preserved after tool result: %#v", blocks)
	}
}

func TestResponsesProxyMovesLeadingAssistantContextIntoSystem(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	provider := &recordingProvider{}
	server := New(cfg, store, provider, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"input": []map[string]any{
			{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": "Earlier answer",
				}},
			},
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{{
					"type": "input_text",
					"text": "New request",
				}},
			},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[0].Messages) != 1 || requests[0].Messages[0].Role != "user" || requests[0].Messages[0].Content != "New request" {
		t.Fatalf("unexpected upstream messages = %#v", requests[0].Messages)
	}
	if len(requests[0].System) != 1 || requests[0].System[0] != "Previous assistant message:\nEarlier answer" {
		t.Fatalf("unexpected upstream system = %#v", requests[0].System)
	}
}

func TestResponsesProxyReturnsUpstreamClientStatus(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, errorProvider{
		err: upstreamStatusError{
			statusCode: http.StatusBadRequest,
			message:    "bedrock converse: validation failed",
		},
	}, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"input": "hello",
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	logs, err := store.ListRequestLogs(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRequestLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected logs = %#v", logs)
	}
}

func TestResponsesProxyToolCallStreaming(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	addTestModelRoute(t, store)
	server := New(cfg, store, &toolProvider{}, "test")
	body := map[string]any{
		"model":  "claude-haiku-4-5",
		"input":  "run pwd",
		"stream": true,
		"tools": []map[string]any{{
			"type": "function",
			"name": "exec_command",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
			},
		}},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, "\"type\":\"response.function_call_arguments.delta\"") {
		t.Fatalf("missing function_call_arguments delta event: %s", bodyText)
	}
	if !strings.Contains(bodyText, "\"type\":\"response.function_call_arguments.done\"") {
		t.Fatalf("missing function_call_arguments done event: %s", bodyText)
	}
}

func TestResponsesDebugLogsRawUpstreamResponse(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	addTestModelRoute(t, store)

	var logs bytes.Buffer
	logger := logging.New("debug", &logs)
	logger = logger.With("component", "test")

	server := NewWithLogger(cfg, store, &toolProvider{}, "test", logger)
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"input": "run pwd",
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	logText := logs.String()
	if !strings.Contains(logText, "responses upstream response") {
		t.Fatalf("missing debug log entry: %s", logText)
	}
	if !strings.Contains(logText, `toolUseId\":\"call_exec_1`) {
		t.Fatalf("missing raw upstream response in log: %s", logText)
	}
	if !strings.Contains(logText, "empty_tool_arguments=false") {
		t.Fatalf("missing tool argument summary in log: %s", logText)
	}
}

func TestResponsesProxyAllowsWebSearchTool(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	addTestModelRoute(t, store)
	provider := &recordingProvider{}
	server := New(cfg, store, provider, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"input": "hello",
		"tools": []map[string]any{
			{
				"type":                "web_search",
				"external_web_access": false,
			},
			{
				"type": "function",
				"name": "exec_command",
				"parameters": map[string]any{
					"type": "object",
				},
			},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "exec_command" {
		t.Fatalf("unexpected upstream tools = %#v", requests[0].Tools)
	}
	var resp struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(resp.Tools) != 2 || resp.Tools[0]["type"] != "web_search" || resp.Tools[1]["type"] != "function" {
		t.Fatalf("unexpected echoed tools = %#v", resp.Tools)
	}
}

func TestResponsesWebSocketProxy(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	addTestModelRoute(t, store)
	server := New(cfg, store, fakeProvider{}, "test")
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/responses"
	header := http.Header{}
	header.Set("Authorization", "Bearer bpx_test_secret")
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			t.Fatalf("Dial() error = %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{
		"type":  "response.create",
		"model": "claude-haiku-4-5",
		"input": "hello",
		"tools": []map[string]any{{
			"type":                "web_search",
			"external_web_access": false,
		}},
	}); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}

	var sawDelta bool
	var sawCompleted bool
	for !sawCompleted {
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("ReadJSON() error = %v", err)
		}
		switch event["type"] {
		case "response.output_text.delta":
			sawDelta = true
		case "response.completed":
			sawCompleted = true
			response, _ := event["response"].(map[string]any)
			if response["output_text"] != "hello from bedrock" {
				t.Fatalf("unexpected output_text = %#v", response["output_text"])
			}
		}
	}
	if !sawDelta {
		t.Fatalf("missing response.output_text.delta event")
	}
}

func TestChatCompletionRateLimitEnforcement(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if err := store.UpsertPricingEntries(context.Background(), []domain.PricingEntry{
		{
			ModelID:          "us.anthropic.claude-haiku-4-5-20251001-v1:0",
			Region:           "us-east-1",
			InputPerMTokens:  1.0,
			OutputPerMTokens: 1.0,
			Version:          "test",
		},
	}); err != nil {
		t.Fatalf("UpsertPricingEntries() error = %v", err)
	}

	limit := 0.0001
	if _, err := store.CreateAPIKey(context.Background(), "limited", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, &limit); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")

	body := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)

	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer bpx_test_secret")
		server.Router().ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over limit request should fail: status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}

	var errResp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if errResp.Error.Type != "rate_limit_error" {
		t.Fatalf("error type = %q, want rate_limit_error", errResp.Error.Type)
	}
}

func TestKeyUsageEndpoint(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	key, err := store.CreateAPIKey(context.Background(), "testkey", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")

	body := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	server.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat request failed: status = %d", rec.Code)
	}

	adminUser := domain.AdminUser{
		Username:     "admin",
		PasswordHash: "irrelevant",
	}
	_ = store.UpsertAdminUser(context.Background(), adminUser)

	loginPayload, _ := json.Marshal(map[string]string{"username": "admin", "password": "test"})
	_ = loginPayload

	monthStr := time.Now().UTC().Format("2006-01")
	usage, err := store.GetAPIKeyMonthlyUsage(context.Background(), key.ID, monthStr)
	if err != nil {
		t.Fatalf("GetAPIKeyMonthlyUsage() error = %v", err)
	}
	if usage == nil {
		t.Fatalf("usage should not be nil")
	}
	if usage.Requests != 1 {
		t.Fatalf("requests = %d, want 1", usage.Requests)
	}
	if usage.TotalTokens != 20 {
		t.Fatalf("total tokens = %d, want 20", usage.TotalTokens)
	}
}

func TestUnknownModelRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")

	body := map[string]any{
		"model": "us.anthropic.claude-3-opus-20240229-v1:0",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown model should be rejected: status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not available") {
		t.Fatalf("error should mention model not available, got: %s", rec.Body.String())
	}
}

func TestDisabledModelRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "disabled-model",
		BedrockModelID: "us.anthropic.claude-3-opus-20240229-v1:0",
		Region:         "us-east-1",
		Enabled:        false,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")

	body := map[string]any{
		"model": "disabled-model",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disabled model should be rejected: status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("error should mention model disabled, got: %s", rec.Body.String())
	}
}

func TestAnthropicMessagesProxy(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	provider := &recordingProvider{}
	server := New(cfg, store, provider, "test")
	body := map[string]any{
		"model":      "claude-haiku-4-5",
		"max_tokens": 256,
		"system": []map[string]any{{
			"type": "text",
			"text": "Be terse.",
		}},
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("X-Api-Key", "bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if resp.Type != "message" || resp.Role != "assistant" || resp.Model != "claude-haiku-4-5" {
		t.Fatalf("unexpected response envelope: %#v", resp)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != "hello from bedrock" {
		t.Fatalf("unexpected content: %#v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 8 {
		t.Fatalf("unexpected usage = %#v", resp.Usage)
	}

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if requests[0].ModelID != "us.anthropic.claude-haiku-4-5-20251001-v1:0" {
		t.Fatalf("model id = %q", requests[0].ModelID)
	}
	if len(requests[0].System) != 1 || requests[0].System[0] != "Be terse." {
		t.Fatalf("system = %#v", requests[0].System)
	}
	if len(requests[0].Messages) != 1 || requests[0].Messages[0].Content != "hello" {
		t.Fatalf("messages = %#v", requests[0].Messages)
	}
	if requests[0].MaxTokens == nil || *requests[0].MaxTokens != 256 {
		t.Fatalf("max tokens = %#v", requests[0].MaxTokens)
	}
}

func TestAnthropicMessagesProxyToolCall(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	provider := &toolProvider{}
	server := New(cfg, store, provider, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "run pwd"},
		},
		"tools": []map[string]any{{
			"name":        "exec_command",
			"description": "Run a command",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []string{"cmd"},
			},
		}},
		"tool_choice": map[string]any{"type": "any"},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("unexpected content: %s", rec.Body.String())
	}
	if resp.Content[0].ID != "call_exec_1" || resp.Content[0].Name != "exec_command" || string(resp.Content[0].Input) != `{"cmd":"pwd"}` {
		t.Fatalf("unexpected tool_use: %#v", resp.Content[0])
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q", resp.StopReason)
	}

	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[0].Tools) != 1 || requests[0].Tools[0].Name != "exec_command" {
		t.Fatalf("tools = %#v", requests[0].Tools)
	}
	if requests[0].ToolChoice == nil || requests[0].ToolChoice.Type != "required" {
		t.Fatalf("tool choice = %#v", requests[0].ToolChoice)
	}
}

func TestAnthropicMessagesProxyToolResult(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	provider := &toolProvider{}
	server := New(cfg, store, provider, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "run pwd"},
			{"role": "assistant", "content": []map[string]any{{
				"type":  "tool_use",
				"id":    "call_exec_1",
				"name":  "exec_command",
				"input": map[string]any{"cmd": "pwd"},
			}}},
			{"role": "user", "content": []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": "call_exec_1",
				"content":     "/Users/personal/broxy",
			}}},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tool completed") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if len(requests[0].Messages) != 3 {
		t.Fatalf("messages = %#v", requests[0].Messages)
	}
	if requests[0].Messages[1].Blocks[0].Type != "tool_use" || requests[0].Messages[2].Blocks[0].Type != "tool_result" {
		t.Fatalf("unexpected tool messages = %#v", requests[0].Messages)
	}
}

func TestAnthropicMessagesProxyStream(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")
	body := map[string]any{
		"model":  "claude-haiku-4-5",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	bodyText := rec.Body.String()
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(bodyText, want) {
			t.Fatalf("missing %q in stream: %s", want, bodyText)
		}
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	tempDir := t.TempDir()
	store, err := db.Open(filepath.Join(tempDir, "proxy.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer store.Close()

	cfg := &config.Config{
		ListenAddr:    "127.0.0.1:0",
		DBPath:        filepath.Join(tempDir, "proxy.db"),
		PricingPath:   filepath.Join(tempDir, "pricing.json"),
		SessionSecret: "0123456789abcdef0123456789abcdef",
		Upstream: config.UpstreamConfig{
			Region: "us-east-1",
		},
	}

	if _, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, nil); err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if _, err := store.UpsertModelRoute(context.Background(), domain.ModelRoute{
		Alias:          "claude-haiku-4-5",
		BedrockModelID: "us.anthropic.claude-haiku-4-5-20251001-v1:0",
		Region:         "us-east-1",
		Enabled:        true,
	}); err != nil {
		t.Fatalf("UpsertModelRoute() error = %v", err)
	}

	server := New(cfg, store, fakeProvider{}, "test")
	body := map[string]any{
		"model": "claude-haiku-4-5",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
		},
	}
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer bpx_test_secret")
	rec := httptest.NewRecorder()
	server.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if resp.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d", resp.InputTokens)
	}
}
