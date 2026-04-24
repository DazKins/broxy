package domain

import "time"

type AppSummary struct {
	StartedAt time.Time `json:"started_at"`
	Version   string    `json:"version"`
}

type AdminUser struct {
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

type APIKey struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	KeyPrefix       string    `json:"key_prefix"`
	ContentLogging  bool      `json:"content_logging"`
	Enabled         bool      `json:"enabled"`
	MonthlyLimitUSD *float64  `json:"monthly_limit_usd,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	LastUsedAt      time.Time `json:"last_used_at,omitempty"`
}

type ModelRoute struct {
	ID                 string    `json:"id"`
	Alias              string    `json:"alias"`
	BedrockModelID     string    `json:"bedrock_model_id"`
	Region             string    `json:"region"`
	Enabled            bool      `json:"enabled"`
	DefaultTemperature *float64  `json:"default_temperature,omitempty"`
	DefaultMaxTokens   *int      `json:"default_max_tokens,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type PricingEntry struct {
	ModelID              string    `json:"model_id"`
	Region               string    `json:"region"`
	InputPerMTokens      float64   `json:"input_per_m_tokens"`
	OutputPerMTokens     float64   `json:"output_per_m_tokens"`
	CacheReadPerMTokens  float64   `json:"cache_read_per_m_tokens"`
	CacheWritePerMTokens float64   `json:"cache_write_per_m_tokens"`
	Version              string    `json:"version"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type RequestRecord struct {
	ID                string    `json:"id"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	APIKeyID          string    `json:"api_key_id"`
	APIKeyName        string    `json:"api_key_name,omitempty"`
	Method            string    `json:"method"`
	Path              string    `json:"path"`
	ModelName         string    `json:"model"`
	BedrockModelID    string    `json:"bedrock_model_id"`
	Region            string    `json:"region"`
	StatusCode        int       `json:"status_code"`
	LatencyMS         int64     `json:"latency_ms"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	TotalTokens       int       `json:"total_tokens"`
	EstimatedCostUSD  float64   `json:"estimated_cost_usd"`
	ContentLogged     bool      `json:"content_logged"`
	RequestJSON       string    `json:"request_json,omitempty"`
	ResponseText      string    `json:"response_text,omitempty"`
	ErrorText         string    `json:"error_text,omitempty"`
	UpstreamRequestID string    `json:"upstream_request_id,omitempty"`
	Stream            bool      `json:"stream"`
}

type UsageBreakdownRow struct {
	BucketDate       string  `json:"bucket_date"`
	ModelName        string  `json:"model"`
	APIKeyName       string  `json:"api_key_name"`
	Requests         int     `json:"requests"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

type DashboardMetrics struct {
	TotalRequests     int64     `json:"total_requests"`
	SuccessRequests   int64     `json:"success_requests"`
	ErrorRequests     int64     `json:"error_requests"`
	TotalInputTokens  int64     `json:"total_input_tokens"`
	TotalOutputTokens int64     `json:"total_output_tokens"`
	TotalCostUSD      float64   `json:"total_cost_usd"`
	AverageLatencyMS  float64   `json:"average_latency_ms"`
	LastRequestAt     time.Time `json:"last_request_at,omitempty"`
}

type AuthenticatedKey struct {
	Key            APIKey
	PlaintextValue string
}

type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  []byte `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`
	CacheHint   bool   `json:"cache_hint,omitempty"`
}

type ToolChoice struct {
	Type         string   `json:"type"`
	Name         string   `json:"name,omitempty"`
	AllowedNames []string `json:"allowed_names,omitempty"`
}

type ToolResultContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	JSON []byte `json:"json,omitempty"`
}

type BedrockContentBlock struct {
	Type             string              `json:"type"`
	Text             string              `json:"text,omitempty"`
	ToolUseID        string              `json:"tool_use_id,omitempty"`
	ToolName         string              `json:"tool_name,omitempty"`
	ToolInput        []byte              `json:"tool_input,omitempty"`
	ToolResultStatus string              `json:"tool_result_status,omitempty"`
	ToolResult       []ToolResultContent `json:"tool_result,omitempty"`
	CacheHint        bool                `json:"cache_hint,omitempty"`
}

type BedrockChatMessage struct {
	Role    string                `json:"role"`
	Content string                `json:"content,omitempty"`
	Blocks  []BedrockContentBlock `json:"blocks,omitempty"`
}

type ConverseRequest struct {
	ModelID          string
	Region           string
	Messages         []BedrockChatMessage
	System           []string
	SystemCacheAfter []int
	Temperature      *float64
	MaxTokens        *int
	Tools            []ToolDefinition
	ToolChoice       *ToolChoice
}

type TokenUsage struct {
	Input      int `json:"input_tokens"`
	Output     int `json:"output_tokens"`
	Total      int `json:"total_tokens"`
	CacheRead  int `json:"cache_read_input_tokens,omitempty"`
	CacheWrite int `json:"cache_write_input_tokens,omitempty"`
}

type ConverseResponse struct {
	ModelID     string             `json:"model_id"`
	Text        string             `json:"text"`
	Message     BedrockChatMessage `json:"message"`
	StopReason  string             `json:"stop_reason"`
	Usage       TokenUsage         `json:"usage"`
	LatencyMS   int64              `json:"latency_ms"`
	RequestID   string             `json:"request_id"`
	RawResponse string             `json:"raw_response,omitempty"`
}

type APIKeyUsageSummary struct {
	APIKeyID         string   `json:"api_key_id"`
	APIKeyName       string   `json:"api_key_name"`
	Month            string   `json:"month"`
	Requests         int      `json:"requests"`
	InputTokens      int      `json:"input_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	TotalTokens      int      `json:"total_tokens"`
	EstimatedCostUSD float64  `json:"estimated_cost_usd"`
	MonthlyLimitUSD  *float64 `json:"monthly_limit_usd,omitempty"`
	IsOverLimit      bool     `json:"is_over_limit"`
}
