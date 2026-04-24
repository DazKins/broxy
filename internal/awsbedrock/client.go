package awsbedrock

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brdocument "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/personal/broxy/internal/config"
	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/logging"
)

type Client struct {
	upstream config.UpstreamConfig
	http     *http.Client
	aws      *bedrockruntime.Client
	logger   *slog.Logger
	awsCfg   *aws.Config
}

type statusError struct {
	statusCode int
	message    string
}

func (e *statusError) Error() string {
	return e.message
}

func (e *statusError) HTTPStatusCode() int {
	return e.statusCode
}

func New(ctx context.Context, upstream config.UpstreamConfig) (*Client, error) {
	return NewWithLogger(ctx, upstream, logging.FromEnv())
}

func NewWithLogger(ctx context.Context, upstream config.UpstreamConfig, logger *slog.Logger) (*Client, error) {
	client := &Client{
		upstream: upstream,
		http: &http.Client{
			Timeout: 5 * time.Minute,
		},
		logger: logger,
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
		client.awsCfg = &cfg
	}
	return client, nil
}

func (c *Client) LogAuth(ctx context.Context) {
	if c.upstream.Mode == config.UpstreamAuthAWS && c.awsCfg != nil {
		c.logAWSAuth(ctx, *c.awsCfg)
	} else if c.upstream.Mode == config.UpstreamAuthBearer {
		c.logBearerAuth()
	}
}

func (c *Client) Converse(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	if c.upstream.Mode == config.UpstreamAuthBearer {
		return c.converseBearer(ctx, req)
	}
	return c.converseAWS(ctx, req)
}

func (c *Client) converseAWS(ctx context.Context, req domain.ConverseRequest) (*domain.ConverseResponse, error) {
	c.logConverseRequest(req)
	messages := make([]brtypes.Message, 0, len(req.Messages))
	for _, msg := range req.Messages {
		content, err := sdkContentBlocks(msg)
		if err != nil {
			return nil, fmt.Errorf("build message content: %w", err)
		}
		role := brtypes.ConversationRoleUser
		if msg.Role == "assistant" {
			role = brtypes.ConversationRoleAssistant
		}
		messages = append(messages, brtypes.Message{
			Role:    role,
			Content: content,
		})
	}
	cacheAfter := make(map[int]struct{}, len(req.SystemCacheAfter))
	for _, i := range req.SystemCacheAfter {
		cacheAfter[i] = struct{}{}
	}
	system := make([]brtypes.SystemContentBlock, 0, len(req.System))
	for i, prompt := range req.System {
		system = append(system, &brtypes.SystemContentBlockMemberText{Value: prompt})
		if _, ok := cacheAfter[i]; ok {
			system = append(system, &brtypes.SystemContentBlockMemberCachePoint{
				Value: brtypes.CachePointBlock{Type: brtypes.CachePointTypeDefault},
			})
		}
	}
	input := &bedrockruntime.ConverseInput{
		ModelId:  &req.ModelID,
		Messages: messages,
		System:   system,
	}
	if len(req.Tools) > 0 {
		toolConfig, err := sdkToolConfig(req.Tools, req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("build tool config: %w", err)
		}
		input.ToolConfig = toolConfig
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
		if out.Usage.CacheReadInputTokens != nil {
			resp.Usage.CacheRead = int(*out.Usage.CacheReadInputTokens)
		}
		if out.Usage.CacheWriteInputTokens != nil {
			resp.Usage.CacheWrite = int(*out.Usage.CacheWriteInputTokens)
		}
	}
	if out.Metrics != nil && out.Metrics.LatencyMs != nil {
		resp.LatencyMS = int64(*out.Metrics.LatencyMs)
	}
	if msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage); ok {
		parsed, err := fromSDKMessage(msg.Value)
		if err != nil {
			return nil, fmt.Errorf("parse converse output: %w", err)
		}
		resp.Message = parsed
		resp.Text = parsedText(parsed.Blocks)
	}
	raw, _ := json.Marshal(out)
	resp.RawResponse = string(raw)
	c.logConverseResponse(req, resp)
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
	payload, err := buildConversePayload(req)
	if err != nil {
		return nil, err
	}
	c.logConverseRequest(req)
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
		return nil, &statusError{
			statusCode: httpResp.StatusCode,
			message:    fmt.Sprintf("bedrock converse status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(respBody))),
		}
	}
	type jsonResponse struct {
		Output struct {
			Message struct {
				Role    string            `json:"role"`
				Content []json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"output"`
		StopReason string `json:"stopReason"`
		Usage      struct {
			InputTokens           int `json:"inputTokens"`
			OutputTokens          int `json:"outputTokens"`
			TotalTokens           int `json:"totalTokens"`
			CacheReadInputTokens  int `json:"cacheReadInputTokens"`
			CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
		} `json:"usage"`
		Metrics struct {
			LatencyMS int64 `json:"latencyMs"`
		} `json:"metrics"`
	}
	var parsed jsonResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse converse response: %w", err)
	}
	blocks, err := fromJSONContentBlocks(parsed.Output.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("parse converse response content: %w", err)
	}
	latency := time.Since(startedAt).Milliseconds()
	if parsed.Metrics.LatencyMS > 0 {
		latency = parsed.Metrics.LatencyMS
	}
	resp := &domain.ConverseResponse{
		ModelID: req.ModelID,
		Text:    parsedText(blocks),
		Message: domain.BedrockChatMessage{
			Role:   parsed.Output.Message.Role,
			Blocks: blocks,
		},
		StopReason: parsed.StopReason,
		Usage: domain.TokenUsage{
			Input:      parsed.Usage.InputTokens,
			Output:     parsed.Usage.OutputTokens,
			Total:      parsed.Usage.TotalTokens,
			CacheRead:  parsed.Usage.CacheReadInputTokens,
			CacheWrite: parsed.Usage.CacheWriteInputTokens,
		},
		LatencyMS:   latency,
		RequestID:   httpResp.Header.Get("x-amzn-requestid"),
		RawResponse: string(respBody),
	}
	c.logConverseResponse(req, resp)
	return resp, nil
}

func sdkContentBlocks(message domain.BedrockChatMessage) ([]brtypes.ContentBlock, error) {
	blocks := message.Blocks
	if len(blocks) == 0 {
		blocks = []domain.BedrockContentBlock{{
			Type: "text",
			Text: message.Content,
		}}
	}
	items := make([]brtypes.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		item, err := sdkContentBlock(block)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if block.CacheHint {
			items = append(items, &brtypes.ContentBlockMemberCachePoint{
				Value: brtypes.CachePointBlock{Type: brtypes.CachePointTypeDefault},
			})
		}
	}
	return items, nil
}

func sdkContentBlock(block domain.BedrockContentBlock) (brtypes.ContentBlock, error) {
	switch block.Type {
	case "", "text":
		return &brtypes.ContentBlockMemberText{Value: block.Text}, nil
	case "tool_use":
		input, err := decodeLazyDocument(block.ToolInput)
		if err != nil {
			return nil, fmt.Errorf("decode tool input: %w", err)
		}
		return &brtypes.ContentBlockMemberToolUse{Value: brtypes.ToolUseBlock{
			Name:      ptrString(block.ToolName),
			ToolUseId: ptrString(block.ToolUseID),
			Input:     input,
		}}, nil
	case "tool_result":
		content := make([]brtypes.ToolResultContentBlock, 0, len(block.ToolResult))
		for _, item := range block.ToolResult {
			switch item.Type {
			case "", "text":
				content = append(content, &brtypes.ToolResultContentBlockMemberText{Value: item.Text})
			case "json":
				value, err := decodeLazyDocument(item.JSON)
				if err != nil {
					return nil, fmt.Errorf("decode tool result json: %w", err)
				}
				content = append(content, &brtypes.ToolResultContentBlockMemberJson{Value: value})
			default:
				return nil, fmt.Errorf("unsupported tool result content type %q", item.Type)
			}
		}
		result := brtypes.ToolResultBlock{
			Content:   content,
			ToolUseId: ptrString(block.ToolUseID),
		}
		switch block.ToolResultStatus {
		case "", "success":
			result.Status = brtypes.ToolResultStatusSuccess
		case "error":
			result.Status = brtypes.ToolResultStatusError
		default:
			return nil, fmt.Errorf("unsupported tool result status %q", block.ToolResultStatus)
		}
		return &brtypes.ContentBlockMemberToolResult{Value: result}, nil
	default:
		return nil, fmt.Errorf("unsupported content block type %q", block.Type)
	}
}

func sdkToolConfig(tools []domain.ToolDefinition, choice *domain.ToolChoice) (*brtypes.ToolConfiguration, error) {
	items := make([]brtypes.Tool, 0, len(tools))
	for _, tool := range tools {
		schemaValue, err := decodeToolSchemaDocument(tool.Parameters)
		if err != nil {
			return nil, fmt.Errorf("decode parameters for %s: %w", tool.Name, err)
		}
		items = append(items, &brtypes.ToolMemberToolSpec{Value: brtypes.ToolSpecification{
			Name:        ptrString(tool.Name),
			Description: ptrString(tool.Description),
			InputSchema: &brtypes.ToolInputSchemaMemberJson{Value: schemaValue},
		}})
		if tool.CacheHint {
			items = append(items, &brtypes.ToolMemberCachePoint{
				Value: brtypes.CachePointBlock{Type: brtypes.CachePointTypeDefault},
			})
		}
	}
	cfg := &brtypes.ToolConfiguration{Tools: items}
	if choice != nil {
		switch choice.Type {
		case "", "auto":
			cfg.ToolChoice = &brtypes.ToolChoiceMemberAuto{Value: brtypes.AutoToolChoice{}}
		case "required":
			cfg.ToolChoice = &brtypes.ToolChoiceMemberAny{Value: brtypes.AnyToolChoice{}}
		case "function":
			cfg.ToolChoice = &brtypes.ToolChoiceMemberTool{Value: brtypes.SpecificToolChoice{Name: ptrString(choice.Name)}}
		}
	}
	return cfg, nil
}

func fromSDKMessage(msg brtypes.Message) (domain.BedrockChatMessage, error) {
	blocks := make([]domain.BedrockContentBlock, 0, len(msg.Content))
	for _, block := range msg.Content {
		item, err := fromSDKContentBlock(block)
		if err != nil {
			return domain.BedrockChatMessage{}, err
		}
		if item.Type != "" {
			blocks = append(blocks, item)
		}
	}
	role := string(msg.Role)
	if role == "" {
		role = "assistant"
	}
	return domain.BedrockChatMessage{
		Role:    role,
		Content: parsedText(blocks),
		Blocks:  blocks,
	}, nil
}

func fromSDKContentBlock(block brtypes.ContentBlock) (domain.BedrockContentBlock, error) {
	switch v := block.(type) {
	case *brtypes.ContentBlockMemberText:
		return domain.BedrockContentBlock{Type: "text", Text: v.Value}, nil
	case *brtypes.ContentBlockMemberToolUse:
		input, err := marshalDocument(v.Value.Input)
		if err != nil {
			return domain.BedrockContentBlock{}, fmt.Errorf("marshal tool input: %w", err)
		}
		return domain.BedrockContentBlock{
			Type:      "tool_use",
			ToolUseID: derefString(v.Value.ToolUseId),
			ToolName:  derefString(v.Value.Name),
			ToolInput: input,
		}, nil
	case *brtypes.ContentBlockMemberToolResult:
		content := make([]domain.ToolResultContent, 0, len(v.Value.Content))
		for _, item := range v.Value.Content {
			switch part := item.(type) {
			case *brtypes.ToolResultContentBlockMemberText:
				content = append(content, domain.ToolResultContent{Type: "text", Text: part.Value})
			case *brtypes.ToolResultContentBlockMemberJson:
				body, err := marshalDocument(part.Value)
				if err != nil {
					return domain.BedrockContentBlock{}, fmt.Errorf("marshal tool result json: %w", err)
				}
				content = append(content, domain.ToolResultContent{Type: "json", JSON: body})
			}
		}
		return domain.BedrockContentBlock{
			Type:             "tool_result",
			ToolUseID:        derefString(v.Value.ToolUseId),
			ToolResultStatus: string(v.Value.Status),
			ToolResult:       content,
		}, nil
	default:
		return domain.BedrockContentBlock{}, nil
	}
}

func fromJSONContentBlocks(raw []json.RawMessage) ([]domain.BedrockContentBlock, error) {
	blocks := make([]domain.BedrockContentBlock, 0, len(raw))
	for _, item := range raw {
		var shape struct {
			Text    *string `json:"text"`
			ToolUse *struct {
				ToolUseID string          `json:"toolUseId"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
			} `json:"toolUse"`
			ToolResult *struct {
				ToolUseID string `json:"toolUseId"`
				Status    string `json:"status"`
				Content   []struct {
					Text *string         `json:"text"`
					JSON json.RawMessage `json:"json"`
				} `json:"content"`
			} `json:"toolResult"`
		}
		if err := json.Unmarshal(item, &shape); err != nil {
			return nil, err
		}
		switch {
		case shape.Text != nil:
			blocks = append(blocks, domain.BedrockContentBlock{Type: "text", Text: *shape.Text})
		case shape.ToolUse != nil:
			blocks = append(blocks, domain.BedrockContentBlock{
				Type:      "tool_use",
				ToolUseID: shape.ToolUse.ToolUseID,
				ToolName:  shape.ToolUse.Name,
				ToolInput: shape.ToolUse.Input,
			})
		case shape.ToolResult != nil:
			content := make([]domain.ToolResultContent, 0, len(shape.ToolResult.Content))
			for _, part := range shape.ToolResult.Content {
				switch {
				case part.Text != nil:
					content = append(content, domain.ToolResultContent{Type: "text", Text: *part.Text})
				case len(part.JSON) > 0 && string(part.JSON) != "null":
					content = append(content, domain.ToolResultContent{Type: "json", JSON: part.JSON})
				}
			}
			blocks = append(blocks, domain.BedrockContentBlock{
				Type:             "tool_result",
				ToolUseID:        shape.ToolResult.ToolUseID,
				ToolResultStatus: shape.ToolResult.Status,
				ToolResult:       content,
			})
		}
	}
	return blocks, nil
}

func parsedText(blocks []domain.BedrockContentBlock) string {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func transformJSONMessages(messages []domain.BedrockChatMessage) []map[string]any {
	items := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		content := make([]map[string]any, 0)
		if len(msg.Blocks) == 0 {
			content = append(content, map[string]any{"text": msg.Content})
		} else {
			for _, block := range msg.Blocks {
				switch block.Type {
				case "", "text":
					content = append(content, map[string]any{"text": block.Text})
				case "tool_use":
					var input any
					if err := json.Unmarshal(nonEmptyJSON(block.ToolInput, []byte(`{}`)), &input); err != nil {
						return nil
					}
					content = append(content, map[string]any{
						"toolUse": map[string]any{
							"toolUseId": block.ToolUseID,
							"name":      block.ToolName,
							"input":     input,
						},
					})
				case "tool_result":
					resultContent := make([]map[string]any, 0, len(block.ToolResult))
					for _, item := range block.ToolResult {
						switch item.Type {
						case "", "text":
							resultContent = append(resultContent, map[string]any{"text": item.Text})
						case "json":
							var value any
							if err := json.Unmarshal(nonEmptyJSON(item.JSON, []byte(`null`)), &value); err != nil {
								return nil
							}
							resultContent = append(resultContent, map[string]any{"json": value})
						}
					}
					toolResult := map[string]any{
						"toolUseId": block.ToolUseID,
						"content":   resultContent,
					}
					if block.ToolResultStatus != "" {
						toolResult["status"] = block.ToolResultStatus
					}
					content = append(content, map[string]any{"toolResult": toolResult})
				}
				if block.CacheHint {
					content = append(content, map[string]any{
						"cachePoint": map[string]any{"type": "default"},
					})
				}
			}
		}
		items = append(items, map[string]any{
			"role":    msg.Role,
			"content": content,
		})
	}
	return items
}

func jsonToolConfig(tools []domain.ToolDefinition, choice *domain.ToolChoice) (map[string]any, error) {
	items := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		params, err := decodeToolSchemaValue(tool.Parameters)
		if err != nil {
			return nil, fmt.Errorf("decode parameters for %s: %w", tool.Name, err)
		}
		spec := map[string]any{
			"name":        tool.Name,
			"inputSchema": map[string]any{"json": params},
		}
		if tool.Description != "" {
			spec["description"] = tool.Description
		}
		if tool.Strict != nil {
			spec["strict"] = *tool.Strict
		}
		items = append(items, map[string]any{"toolSpec": spec})
		if tool.CacheHint {
			items = append(items, map[string]any{
				"cachePoint": map[string]any{"type": "default"},
			})
		}
	}
	cfg := map[string]any{"tools": items}
	if choice != nil {
		switch choice.Type {
		case "", "auto":
			cfg["toolChoice"] = map[string]any{"auto": map[string]any{}}
		case "required":
			cfg["toolChoice"] = map[string]any{"any": map[string]any{}}
		case "function":
			cfg["toolChoice"] = map[string]any{"tool": map[string]any{"name": choice.Name}}
		}
	}
	return cfg, nil
}

func buildConversePayload(req domain.ConverseRequest) (map[string]any, error) {
	payload := map[string]any{
		"messages": transformJSONMessages(req.Messages),
	}
	if len(req.System) > 0 {
		cacheAfter := make(map[int]struct{}, len(req.SystemCacheAfter))
		for _, i := range req.SystemCacheAfter {
			cacheAfter[i] = struct{}{}
		}
		system := make([]map[string]any, 0, len(req.System))
		for i, prompt := range req.System {
			system = append(system, map[string]any{"text": prompt})
			if _, ok := cacheAfter[i]; ok {
				system = append(system, map[string]any{
					"cachePoint": map[string]any{"type": "default"},
				})
			}
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
	if len(req.Tools) > 0 {
		toolConfig, err := jsonToolConfig(req.Tools, req.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("build tool config: %w", err)
		}
		payload["toolConfig"] = toolConfig
	}
	return payload, nil
}

func decodeLazyDocument(raw []byte) (brdocument.Interface, error) {
	if len(raw) == 0 {
		return brdocument.NewLazyDocument(map[string]any{}), nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return brdocument.NewLazyDocument(value), nil
}

func decodeToolSchemaDocument(raw []byte) (brdocument.Interface, error) {
	value, err := decodeToolSchemaValue(raw)
	if err != nil {
		return nil, err
	}
	return brdocument.NewLazyDocument(value), nil
}

func decodeToolSchemaValue(raw []byte) (any, error) {
	var value any
	if err := json.Unmarshal(nonEmptyJSON(raw, []byte(`{"type":"object","properties":{}}`)), &value); err != nil {
		return nil, err
	}
	normalizeToolSchema(value)
	return value, nil
}

func normalizeToolSchema(value any) {
	switch schema := value.(type) {
	case map[string]any:
		if schemaHasObjectType(schema) {
			if _, ok := schema["additionalProperties"]; !ok {
				schema["additionalProperties"] = false
			}
		}
		for _, key := range []string{"properties", "$defs", "definitions", "dependentSchemas"} {
			if children, ok := schema[key].(map[string]any); ok {
				for _, child := range children {
					normalizeToolSchema(child)
				}
			}
		}
		for _, key := range []string{"items", "additionalItems", "contains", "propertyNames", "if", "then", "else", "not"} {
			if child, ok := schema[key]; ok {
				normalizeToolSchema(child)
			}
		}
		for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
			if children, ok := schema[key].([]any); ok {
				for _, child := range children {
					normalizeToolSchema(child)
				}
			}
		}
	case []any:
		for _, item := range schema {
			normalizeToolSchema(item)
		}
	}
}

func schemaHasObjectType(schema map[string]any) bool {
	switch value := schema["type"].(type) {
	case string:
		return value == "object"
	case []any:
		for _, item := range value {
			if item == "object" {
				return true
			}
		}
	}
	return false
}

func marshalDocument(value brdocument.Interface) ([]byte, error) {
	if value == nil {
		return []byte(`null`), nil
	}
	body, err := value.MarshalSmithyDocument()
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return []byte(`null`), nil
	}
	return body, nil
}

func ptrString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nonEmptyJSON(raw []byte, fallback []byte) []byte {
	if len(raw) == 0 {
		return fallback
	}
	return raw
}

func (c *Client) logConverseResponse(req domain.ConverseRequest, resp *domain.ConverseResponse) {
	if c.logger == nil {
		return
	}
	c.logger.Debug("bedrock converse response",
		"model_id", req.ModelID,
		"region", req.Region,
		"stop_reason", resp.StopReason,
		"input_tokens", resp.Usage.Input,
		"output_tokens", resp.Usage.Output,
		"total_tokens", resp.Usage.Total,
		"latency_ms", resp.LatencyMS,
		"request_id", resp.RequestID,
		"text_bytes", len(resp.Text),
		"message_blocks", len(resp.Message.Blocks),
		"raw_response", resp.RawResponse,
	)
}

func (c *Client) logConverseRequest(req domain.ConverseRequest) {
	if c.logger == nil {
		return
	}
	payload, err := buildConversePayload(req)
	if err != nil {
		c.logger.Debug("bedrock converse request",
			"model_id", req.ModelID,
			"region", req.Region,
			"tool_count", len(req.Tools),
			"message_count", len(req.Messages),
			"system_count", len(req.System),
			"tool_choice", req.ToolChoice,
			"payload_error", err.Error(),
		)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.logger.Debug("bedrock converse request",
			"model_id", req.ModelID,
			"region", req.Region,
			"tool_count", len(req.Tools),
			"message_count", len(req.Messages),
			"system_count", len(req.System),
			"tool_choice", req.ToolChoice,
			"payload_error", err.Error(),
		)
		return
	}
	c.logger.Debug("bedrock converse request",
		"model_id", req.ModelID,
		"region", req.Region,
		"tool_count", len(req.Tools),
		"message_count", len(req.Messages),
		"system_count", len(req.System),
		"tool_choice", req.ToolChoice,
		"raw_request", string(body),
	)
}
