package awsbedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/domain"
)

type Client struct {
	upstream config.UpstreamConfig
	http     *http.Client
	aws      *bedrockruntime.Client
}

func New(ctx context.Context, upstream config.UpstreamConfig) (*Client, error) {
	client := &Client{
		upstream: upstream,
		http: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
	if upstream.Mode == config.UpstreamAuthAWS {
		loadOptions := []func(*awsconfig.LoadOptions) error{
			awsconfig.WithRegion(upstream.Region),
		}
		if upstream.Profile != "" {
			loadOptions = append(loadOptions, awsconfig.WithSharedConfigProfile(upstream.Profile))
		}
		cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
		if err != nil {
			return nil, fmt.Errorf("load aws config: %w", err)
		}
		client.aws = bedrockruntime.NewFromConfig(cfg)
	}
	return client, nil
}

func (c *Client) Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	if c.upstream.Mode == config.UpstreamAuthBearer {
		return c.converseBearer(ctx, req)
	}
	return c.converseAWS(ctx, req)
}

func (c *Client) converseAWS(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	messages := make([]brtypes.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		role := brtypes.ConversationRoleUser
		if msg.Role == "assistant" {
			role = brtypes.ConversationRoleAssistant
		}
		messages = append(messages, brtypes.Message{
			Role: role,
			Content: []brtypes.ContentBlock{
				&brtypes.ContentBlockMemberText{Value: msg.Content},
			},
		})
	}
	system := make([]brtypes.SystemContentBlock, 0, len(req.System))
	for _, prompt := range req.System {
		system = append(system, &brtypes.SystemContentBlockMemberText{Value: prompt})
	}
	input := &bedrockruntime.ConverseInput{
		ModelId:  &req.ModelID,
		Messages: messages,
		System:   system,
	}
	if req.MaxTokens != nil || req.Temperature != nil {
		input.InferenceConfig = &brtypes.InferenceConfiguration{}
		if req.MaxTokens != nil {
			maxTokens := int32(*req.MaxTokens)
			input.InferenceConfig.MaxTokens = &maxTokens
		}
		if req.Temperature != nil {
			temp := float32(*req.Temperature)
			input.InferenceConfig.Temperature = &temp
		}
	}
	startedAt := time.Now()
	out, err := c.aws.Converse(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("bedrock converse: %w", err)
	}
	resp := &domain.ConverseResponse{
		ModelID:    req.ModelID,
		StopReason: string(out.StopReason),
		LatencyMS:  time.Since(startedAt).Milliseconds(),
	}
	if out.Usage != nil {
		resp.Usage.Input = int(*out.Usage.InputTokens)
		resp.Usage.Output = int(*out.Usage.OutputTokens)
		resp.Usage.Total = int(*out.Usage.TotalTokens)
	}
	if out.Metrics != nil && out.Metrics.LatencyMs != nil {
		resp.LatencyMS = int64(*out.Metrics.LatencyMs)
	}
	if msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage); ok {
		resp.Text = flattenSDKContent(msg.Value.Content)
	}
	raw, _ := json.Marshal(out)
	resp.RawResponse = string(raw)
	return resp, nil
}

func (c *Client) converseBearer(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	if strings.TrimSpace(c.upstream.BearerToken) == "" {
		return nil, fmt.Errorf("bedrock bearer mode selected but no bearer token configured")
	}
	endpoint := c.upstream.EndpointOverride
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", req.Region)
	}
	u, err := url.JoinPath(endpoint, "model", req.ModelID, "converse")
	if err != nil {
		return nil, fmt.Errorf("build bedrock URL: %w", err)
	}
	payload := map[string]any{
		"messages": transformJSONMessages(req.Messages),
	}
	if len(req.System) > 0 {
		system := make([]map[string]string, 0, len(req.System))
		for _, prompt := range req.System {
			system = append(system, map[string]string{"text": prompt})
		}
		payload["system"] = system
	}
	if req.MaxTokens != nil || req.Temperature != nil {
		cfg := map[string]any{}
		if req.MaxTokens != nil {
			cfg["maxTokens"] = *req.MaxTokens
		}
		if req.Temperature != nil {
			cfg["temperature"] = *req.Temperature
		}
		payload["inferenceConfig"] = cfg
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal converse payload: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build converse request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.upstream.BearerToken)
	startedAt := time.Now()
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("do converse request: %w", err)
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read converse response: %w", err)
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("bedrock converse status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	type jsonResponse struct {
		Output struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string `json:"stopReason"`
		Usage      struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
			TotalTokens  int `json:"totalTokens"`
		} `json:"usage"`
		Metrics struct {
			LatencyMS int64 `json:"latencyMs"`
		} `json:"metrics"`
	}
	var parsed jsonResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse converse response: %w", err)
	}
	var builder strings.Builder
	for _, part := range parsed.Output.Message.Content {
		builder.WriteString(part.Text)
	}
	latency := time.Since(startedAt).Milliseconds()
	if parsed.Metrics.LatencyMS > 0 {
		latency = parsed.Metrics.LatencyMS
	}
	return &domain.ConverseResponse{
		ModelID:    req.ModelID,
		Text:       builder.String(),
		StopReason: parsed.StopReason,
		Usage: domain.TokenUsage{
			Input:  parsed.Usage.InputTokens,
			Output: parsed.Usage.OutputTokens,
			Total:  parsed.Usage.TotalTokens,
		},
		LatencyMS:   latency,
		RequestID:   httpResp.Header.Get("x-amzn-requestid"),
		RawResponse: string(respBody),
	}, nil
}

func flattenSDKContent(blocks []brtypes.ContentBlock) string {
	var builder strings.Builder
	for _, block := range blocks {
		switch v := block.(type) {
		case *brtypes.ContentBlockMemberText:
			builder.WriteString(v.Value)
		}
	}
	return builder.String()
}

func transformJSONMessages(messages []domain.BedrockChatMessage) []map[string]any {
	items := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		items = append(items, map[string]any{
			"role": msg.Role,
			"content": []map[string]string{
				{"text": msg.Content},
			},
		})
	}
	return items
}
