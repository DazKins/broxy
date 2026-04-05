package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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

	key, err := store.CreateAPIKey(context.Background(), "tests", "bpx_test", security.HashAPIKey("bpx_test_secret"), true)
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
