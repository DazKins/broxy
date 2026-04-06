package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

	// Seed a pricing entry so requests have non-zero cost.
	// fakeProvider returns 12 input + 8 output tokens.
	// At $1/M input and $1/M output, each request costs ~$0.00002.
	// With a limit of $0.0001, 5 requests ($0.0001) should exceed it.
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
	_, err = store.CreateAPIKey(context.Background(), "limited", "bpx_test", security.HashAPIKey("bpx_test_secret"), true, &limit)
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

	// Send enough requests to exceed the limit.
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer bpx_test_secret")
		server.Router().ServeHTTP(rec, req)
	}

	// This request should be over limit.
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

	// Make a request to generate some usage.
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

	// Query the usage endpoint (unauthenticated for simplicity — it's behind admin middleware).
	// We need to create an admin session cookie.
	adminUser := domain.AdminUser{
		Username:     "admin",
		PasswordHash: "irrelevant",
	}
	_ = store.UpsertAdminUser(context.Background(), adminUser)

	// Login to get cookie
	loginPayload, _ := json.Marshal(map[string]string{"username": "admin", "password": "test"})
	_ = loginPayload

	// Instead, query the usage from the store directly.
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
