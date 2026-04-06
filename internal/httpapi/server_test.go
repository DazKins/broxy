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

	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/db"
	"github.com/personal/broxy/internal/domain"
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
