package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/personal/broxy/internal/domain"
	"github.com/personal/broxy/internal/pricing"
	searchpkg "github.com/personal/broxy/internal/search"
)

type AnthropicMessagesRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage    `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Type           string          `json:"type,omitempty"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	MaxUses        int             `json:"max_uses,omitempty"`
	AllowedDomains []string        `json:"allowed_domains,omitempty"`
	BlockedDomains []string        `json:"blocked_domains,omitempty"`
	CacheControl   json.RawMessage `json:"cache_control,omitempty"`
}

type normalizedAnthropicMessagesRequest struct {
	Messages         []domain.BedrockChatMessage
	System           []string
	SystemCacheAfter []int
	Tools            []domain.ToolDefinition
	ToolChoice       *domain.ToolChoice
	WebSearch        *anthropicWebSearchConfig
}

type anthropicWebSearchConfig struct {
	MaxUses        int
	AllowedDomains []string
	BlockedDomains []string
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	apiKey := clientKeyFromContext(r.Context())
	if apiKey == nil {
		writeError(w, http.StatusUnauthorized, "missing client authentication")
		return
	}
	startedAt := time.Now().UTC()
	var req AnthropicMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	response, status, err := s.processAnthropicMessagesRequest(r.Context(), apiKey, r.Method, r.URL.Path, req, startedAt)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if req.Stream {
		s.streamAnthropicMessage(w, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	var req AnthropicMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	_, _, _, _, _, err := s.resolveModel(r.Context(), req.Model, req.Temperature, req.MaxTokens)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	normalized, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": estimateAnthropicInputTokens(normalized, req),
	})
}

func (s *Server) processAnthropicMessagesRequest(ctx context.Context, apiKey *domain.APIKey, method string, path string, req AnthropicMessagesRequest, startedAt time.Time) (map[string]any, int, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, http.StatusBadRequest, errors.New("model is required")
	}
	normalized, err := normalizeAnthropicMessagesRequest(req)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	route, bedrockModelID, region, temp, maxTokens, err := s.resolveModel(ctx, req.Model, req.Temperature, req.MaxTokens)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	converseReq := domain.ConverseRequest{
		ModelID:          bedrockModelID,
		Region:           region,
		Messages:         normalized.Messages,
		System:           normalized.System,
		SystemCacheAfter: normalized.SystemCacheAfter,
		Temperature:      temp,
		MaxTokens:        maxTokens,
		Tools:            normalized.Tools,
		ToolChoice:       normalized.ToolChoice,
	}
	if normalized.ToolChoice != nil && normalized.ToolChoice.Type == "none" {
		converseReq.Tools = nil
		converseReq.ToolChoice = nil
		normalized.WebSearch = nil
	}
	upstreamResp, err := s.completeAnthropicMessages(ctx, converseReq, normalized.WebSearch)
	finishedAt := time.Now().UTC()
	if err != nil {
		statusCode := upstreamStatusCode(err)
		s.logRequest(ctx, domain.RequestRecord{
			StartedAt:      startedAt,
			FinishedAt:     finishedAt,
			APIKeyID:       apiKey.ID,
			Method:         method,
			Path:           path,
			ModelName:      req.Model,
			BedrockModelID: bedrockModelID,
			Region:         region,
			StatusCode:     statusCode,
			LatencyMS:      finishedAt.Sub(startedAt).Milliseconds(),
			ErrorText:      err.Error(),
			ContentLogged:  apiKey.ContentLogging,
			RequestJSON:    s.maybeLoggedJSON(apiKey.ContentLogging, req),
			Stream:         req.Stream,
		})
		return nil, statusCode, err
	}
	costEntry, _ := s.store.GetPricingEntry(ctx, upstreamResp.ModelID, region)
	cost := pricing.EstimateCost(costEntry, upstreamResp.Usage)
	record := domain.RequestRecord{
		StartedAt:         startedAt,
		FinishedAt:        finishedAt,
		APIKeyID:          apiKey.ID,
		Method:            method,
		Path:              path,
		ModelName:         coalesceRouteName(route, req.Model),
		BedrockModelID:    upstreamResp.ModelID,
		Region:            region,
		StatusCode:        http.StatusOK,
		LatencyMS:         upstreamResp.LatencyMS,
		InputTokens:       upstreamResp.Usage.Input,
		OutputTokens:      upstreamResp.Usage.Output,
		TotalTokens:       upstreamResp.Usage.Total,
		EstimatedCostUSD:  cost,
		ContentLogged:     apiKey.ContentLogging,
		RequestJSON:       s.maybeLoggedJSON(apiKey.ContentLogging, req),
		ResponseText:      s.maybeLogText(apiKey.ContentLogging, upstreamResp.Text),
		UpstreamRequestID: upstreamResp.RequestID,
		Stream:            req.Stream,
	}
	defer s.logRequest(ctx, record)

	return buildAnthropicMessageEnvelope(req.Model, upstreamResp), http.StatusOK, nil
}

func (s *Server) completeAnthropicMessages(ctx context.Context, req domain.ConverseRequest, webSearch *anthropicWebSearchConfig) (*domain.ConverseResponse, error) {
	if webSearch == nil {
		return s.provider.Converse(ctx, req)
	}
	if s.search == nil {
		message := unconfiguredAnthropicWebSearchMessage()
		return &domain.ConverseResponse{
			ModelID: req.ModelID,
			Text:    message,
			Message: domain.BedrockChatMessage{
				Role: "assistant",
				Blocks: []domain.BedrockContentBlock{{
					Type: "text",
					Text: message,
				}},
			},
			StopReason: "end_turn",
		}, nil
	}

	req.Tools = append([]domain.ToolDefinition{anthropicWebSearchToolDefinition()}, req.Tools...)
	if req.ToolChoice == nil {
		req.ToolChoice = &domain.ToolChoice{Type: "auto"}
	}
	maxUses := webSearch.MaxUses
	if maxUses <= 0 {
		maxUses = 5
	}
	var totalUsage domain.TokenUsage
	var totalLatency int64
	searchUses := 0
	for turns := 0; turns < maxUses+2; turns++ {
		resp, err := s.provider.Converse(ctx, req)
		if err != nil {
			return nil, err
		}
		totalUsage.Input += resp.Usage.Input
		totalUsage.Output += resp.Usage.Output
		totalUsage.Total += resp.Usage.Total
		totalLatency += resp.LatencyMS

		webToolUses, hasOtherToolUse := anthropicWebSearchToolUses(resp.Message.Blocks)
		if len(webToolUses) == 0 || hasOtherToolUse {
			resp.Usage = totalUsage
			resp.LatencyMS = totalLatency
			return resp, nil
		}

		req.Messages = append(req.Messages, resp.Message)
		resultBlocks := make([]domain.BedrockContentBlock, 0, len(webToolUses))
		for _, block := range webToolUses {
			if searchUses >= maxUses {
				resultBlocks = append(resultBlocks, anthropicWebSearchErrorResult(block.ToolUseID, "max_uses_exceeded"))
				continue
			}
			searchUses++
			resultBlocks = append(resultBlocks, s.runAnthropicWebSearch(ctx, block, webSearch))
		}
		req.Messages = append(req.Messages, domain.BedrockChatMessage{
			Role:   "user",
			Blocks: resultBlocks,
		})
	}
	message := "Web search stopped because the model exceeded Broxy's web search turn limit."
	return &domain.ConverseResponse{
		ModelID: req.ModelID,
		Text:    message,
		Message: domain.BedrockChatMessage{
			Role: "assistant",
			Blocks: []domain.BedrockContentBlock{{
				Type: "text",
				Text: message,
			}},
		},
		StopReason: "end_turn",
		Usage:      totalUsage,
		LatencyMS:  totalLatency,
	}, nil
}

func (s *Server) runAnthropicWebSearch(ctx context.Context, block domain.BedrockContentBlock, cfg *anthropicWebSearchConfig) domain.BedrockContentBlock {
	query, err := anthropicWebSearchQuery(block.ToolInput)
	if err != nil {
		return anthropicWebSearchErrorResult(block.ToolUseID, err.Error())
	}
	resp, err := s.search.Search(ctx, searchpkg.Request{
		Query:          query,
		AllowedDomains: cfg.AllowedDomains,
		BlockedDomains: cfg.BlockedDomains,
	})
	if err != nil {
		return anthropicWebSearchErrorResult(block.ToolUseID, err.Error())
	}
	return domain.BedrockContentBlock{
		Type:      "tool_result",
		ToolUseID: block.ToolUseID,
		ToolResult: []domain.ToolResultContent{{
			Type: "text",
			Text: searchpkg.FormatResponse(resp),
		}},
	}
}

func normalizeAnthropicMessagesRequest(req AnthropicMessagesRequest) (*normalizedAnthropicMessagesRequest, error) {
	system, systemCacheAfter, err := parseAnthropicSystem(req.System)
	if err != nil {
		return nil, err
	}
	messages := make([]domain.BedrockChatMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}
		blocks, err := parseAnthropicContentBlocks(msg.Content)
		if err != nil {
			return nil, err
		}
		appendResponseMessage(&messages, role, blocks...)
	}
	messages, system, err = normalizeBedrockConversation(messages, system)
	if err != nil {
		return nil, err
	}
	tools, webSearch, err := parseAnthropicTools(req.Tools)
	if err != nil {
		return nil, err
	}
	choice, err := parseAnthropicToolChoice(req.ToolChoice, len(tools) > 0 || webSearch != nil)
	if err != nil {
		return nil, err
	}
	return &normalizedAnthropicMessagesRequest{
		Messages:         messages,
		System:           system,
		SystemCacheAfter: systemCacheAfter,
		Tools:            tools,
		ToolChoice:       choice,
		WebSearch:        webSearch,
	}, nil
}

func parseAnthropicSystem(raw json.RawMessage) ([]string, []int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		if strings.TrimSpace(direct) == "" {
			return nil, nil, nil
		}
		return []string{direct}, nil, nil
	}
	var parts []struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, fmt.Errorf("unsupported system shape")
	}
	system := make([]string, 0, len(parts))
	var cacheAfter []int
	for _, part := range parts {
		switch part.Type {
		case "", "text":
			if strings.TrimSpace(part.Text) == "" {
				continue
			}
			system = append(system, part.Text)
			if hasAnthropicCacheControl(part.CacheControl) {
				cacheAfter = append(cacheAfter, len(system)-1)
			}
		default:
			return nil, nil, fmt.Errorf("unsupported system content type %q", part.Type)
		}
	}
	return system, cacheAfter, nil
}

func hasAnthropicCacheControl(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return strings.TrimSpace(obj.Type) != ""
}

func parseAnthropicContentBlocks(raw json.RawMessage) ([]domain.BedrockContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return []domain.BedrockContentBlock{{Type: "text", Text: direct}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("unsupported message content shape")
	}
	blocks := make([]domain.BedrockContentBlock, 0, len(parts))
	for _, part := range parts {
		var block struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			Input        json.RawMessage `json:"input"`
			ToolUseID    string          `json:"tool_use_id"`
			Content      json.RawMessage `json:"content"`
			IsError      bool            `json:"is_error"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if err := json.Unmarshal(part, &block); err != nil {
			return nil, fmt.Errorf("unsupported content block shape")
		}
		cacheHint := hasAnthropicCacheControl(block.CacheControl)
		switch block.Type {
		case "", "text":
			blocks = append(blocks, domain.BedrockContentBlock{Type: "text", Text: block.Text, CacheHint: cacheHint})
		case "tool_use":
			blocks = append(blocks, domain.BedrockContentBlock{
				Type:      "tool_use",
				ToolUseID: block.ID,
				ToolName:  block.Name,
				ToolInput: append([]byte(nil), nonEmptyJSON(block.Input, []byte(`{}`))...),
				CacheHint: cacheHint,
			})
		case "tool_result":
			result, err := parseAnthropicToolResultContent(block.Content)
			if err != nil {
				return nil, err
			}
			status := "success"
			if block.IsError {
				status = "error"
			}
			blocks = append(blocks, domain.BedrockContentBlock{
				Type:             "tool_result",
				ToolUseID:        block.ToolUseID,
				ToolResultStatus: status,
				ToolResult:       result,
				CacheHint:        cacheHint,
			})
		case "thinking", "redacted_thinking":
			continue
		default:
			return nil, fmt.Errorf("unsupported content block type %q", block.Type)
		}
	}
	return blocks, nil
}

func parseAnthropicToolResultContent(raw json.RawMessage) ([]domain.ToolResultContent, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return []domain.ToolResultContent{{Type: "text", Text: direct}}, nil
	}
	var parts []struct {
		Type string          `json:"type"`
		Text string          `json:"text"`
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("unsupported tool_result content shape")
	}
	content := make([]domain.ToolResultContent, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "", "text":
			content = append(content, domain.ToolResultContent{Type: "text", Text: part.Text})
		case "json":
			content = append(content, domain.ToolResultContent{Type: "json", JSON: append([]byte(nil), nonEmptyJSON(part.JSON, []byte(`null`))...)})
		default:
			return nil, fmt.Errorf("unsupported tool_result content type %q", part.Type)
		}
	}
	return content, nil
}

func parseAnthropicTools(tools []anthropicTool) ([]domain.ToolDefinition, *anthropicWebSearchConfig, error) {
	defs := make([]domain.ToolDefinition, 0, len(tools))
	var webSearch *anthropicWebSearchConfig
	for _, tool := range tools {
		if isAnthropicWebSearchToolType(tool.Type) {
			if webSearch == nil {
				webSearch = &anthropicWebSearchConfig{}
			}
			if tool.MaxUses > 0 {
				webSearch.MaxUses = tool.MaxUses
			}
			webSearch.AllowedDomains = append(webSearch.AllowedDomains, tool.AllowedDomains...)
			webSearch.BlockedDomains = append(webSearch.BlockedDomains, tool.BlockedDomains...)
			continue
		}
		if tool.Type != "" && tool.Type != "custom" {
			return nil, nil, fmt.Errorf("unsupported tool type %q", tool.Type)
		}
		if strings.TrimSpace(tool.Name) == "" {
			return nil, nil, errors.New("tool name is required")
		}
		defs = append(defs, domain.ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  nonEmptyJSON(tool.InputSchema, []byte(`{"type":"object","properties":{}}`)),
			CacheHint:   hasAnthropicCacheControl(tool.CacheControl),
		})
	}
	return defs, webSearch, nil
}

func isAnthropicWebSearchToolType(toolType string) bool {
	toolType = strings.TrimSpace(toolType)
	return toolType == "web_search" || strings.HasPrefix(toolType, "web_search_")
}

func anthropicWebSearchToolDefinition() domain.ToolDefinition {
	return domain.ToolDefinition{
		Name:        "web_search",
		Description: "Search the web for current information. Use this when answering requires up-to-date or source-backed information.",
		Parameters:  []byte(`{"type":"object","properties":{"query":{"type":"string","description":"Search query to send to the web search provider."}},"required":["query"],"additionalProperties":false}`),
	}
}

func anthropicWebSearchToolUses(blocks []domain.BedrockContentBlock) ([]domain.BedrockContentBlock, bool) {
	var web []domain.BedrockContentBlock
	var other bool
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		if block.ToolName == "web_search" {
			web = append(web, block)
			continue
		}
		other = true
	}
	return web, other
}

func anthropicWebSearchQuery(raw json.RawMessage) (string, error) {
	var input struct {
		Query string `json:"query"`
		Q     string `json:"q"`
	}
	if err := json.Unmarshal(nonEmptyJSON(raw, []byte(`{}`)), &input); err != nil {
		return "", fmt.Errorf("invalid web_search input: %w", err)
	}
	query := strings.TrimSpace(input.Query)
	if query == "" {
		query = strings.TrimSpace(input.Q)
	}
	if query == "" {
		return "", errors.New("web_search query is required")
	}
	return query, nil
}

func anthropicWebSearchErrorResult(toolUseID string, message string) domain.BedrockContentBlock {
	return domain.BedrockContentBlock{
		Type:             "tool_result",
		ToolUseID:        toolUseID,
		ToolResultStatus: "error",
		ToolResult: []domain.ToolResultContent{{
			Type: "text",
			Text: message,
		}},
	}
}

func parseAnthropicToolChoice(raw json.RawMessage, hasTools bool) (*domain.ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if hasTools {
			return &domain.ToolChoice{Type: "auto"}, nil
		}
		return nil, nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil, fmt.Errorf("invalid tool_choice: %w", err)
	}
	switch choice.Type {
	case "auto":
		return &domain.ToolChoice{Type: "auto"}, nil
	case "any", "required":
		return &domain.ToolChoice{Type: "required"}, nil
	case "tool", "function":
		if strings.TrimSpace(choice.Name) == "" {
			return nil, errors.New("tool_choice name is required")
		}
		return &domain.ToolChoice{Type: "function", Name: choice.Name}, nil
	case "none":
		return &domain.ToolChoice{Type: "none"}, nil
	default:
		return nil, fmt.Errorf("unsupported tool_choice type %q", choice.Type)
	}
}

func buildAnthropicMessageEnvelope(model string, resp *domain.ConverseResponse) map[string]any {
	content := buildAnthropicContent(resp)
	usage := map[string]any{
		"input_tokens":  resp.Usage.Input,
		"output_tokens": resp.Usage.Output,
	}
	if resp.Usage.CacheRead > 0 {
		usage["cache_read_input_tokens"] = resp.Usage.CacheRead
	}
	if resp.Usage.CacheWrite > 0 {
		usage["cache_creation_input_tokens"] = resp.Usage.CacheWrite
	}
	return map[string]any{
		"id":            "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopReason(resp.StopReason),
		"stop_sequence": nil,
		"usage":         usage,
	}
}

func unconfiguredAnthropicWebSearchMessage() string {
	return "Broxy is being used as an AWS Bedrock proxy. Claude Code requested web search, but no Broxy search provider is configured. Add a search block to the Broxy config with provider \"brave\" and your Brave Search API key, then retry."
}

func buildAnthropicContent(resp *domain.ConverseResponse) []map[string]any {
	message := resp.Message
	if len(message.Blocks) == 0 && message.Content == "" && resp.Text != "" {
		message.Content = resp.Text
	}
	if len(message.Blocks) == 0 && message.Content != "" {
		message.Blocks = []domain.BedrockContentBlock{{Type: "text", Text: message.Content}}
	}
	content := make([]map[string]any, 0, len(message.Blocks))
	for _, block := range message.Blocks {
		switch block.Type {
		case "", "text":
			content = append(content, map[string]any{
				"type": "text",
				"text": block.Text,
			})
		case "tool_use":
			var input any
			if err := json.Unmarshal(nonEmptyJSON(block.ToolInput, []byte(`{}`)), &input); err != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    block.ToolUseID,
				"name":  block.ToolName,
				"input": input,
			})
		}
	}
	return content
}

func anthropicStopReason(value string) string {
	switch value {
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	case "", "end_turn":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func (s *Server) streamAnthropicMessage(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	writeAnthropicSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            response["id"],
			"type":          "message",
			"role":          "assistant",
			"model":         response["model"],
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  responseUsageInt(response, "input_tokens"),
				"output_tokens": 0,
			},
		},
	})
	flusher.Flush()

	content, _ := response["content"].([]map[string]any)
	for index, block := range content {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			writeAnthropicSSE(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			text, _ := block["text"].(string)
			for _, chunk := range chunkText(text, 48) {
				writeAnthropicSSE(w, "content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{
						"type": "text_delta",
						"text": chunk,
					},
				})
			}
		case "tool_use":
			writeAnthropicSSE(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    block["id"],
					"name":  block["name"],
					"input": map[string]any{},
				},
			})
			inputBody, _ := json.Marshal(block["input"])
			writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": string(inputBody),
				},
			})
		}
		writeAnthropicSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		})
		flusher.Flush()
	}
	writeAnthropicSSE(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   response["stop_reason"],
			"stop_sequence": response["stop_sequence"],
		},
		"usage": map[string]any{
			"output_tokens": responseUsageInt(response, "output_tokens"),
		},
	})
	writeAnthropicSSE(w, "message_stop", map[string]any{
		"type": "message_stop",
	})
	flusher.Flush()
}

func writeAnthropicSSE(w http.ResponseWriter, event string, value any) {
	body, _ := json.Marshal(value)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
}

func responseUsageInt(response map[string]any, key string) int {
	usage, _ := response["usage"].(map[string]any)
	switch value := usage[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

func estimateAnthropicInputTokens(normalized *normalizedAnthropicMessagesRequest, req AnthropicMessagesRequest) int {
	chars := 0
	for _, item := range normalized.System {
		chars += len(item)
	}
	for _, msg := range normalized.Messages {
		chars += len(msg.Role)
		for _, block := range msg.Blocks {
			chars += len(block.Text) + len(block.ToolUseID) + len(block.ToolName) + len(block.ToolInput)
			for _, result := range block.ToolResult {
				chars += len(result.Text) + len(result.JSON)
			}
		}
	}
	for _, tool := range req.Tools {
		chars += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}
	tokens := chars / 4
	if chars%4 != 0 {
		tokens++
	}
	if tokens < 1 {
		return 1
	}
	return tokens
}
