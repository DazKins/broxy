package httpapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/personal/broxy/internal/domain"
)

type ResponseRequest struct {
	Model                string          `json:"model"`
	Input                json.RawMessage `json:"input,omitempty"`
	Instructions         json.RawMessage `json:"instructions,omitempty"`
	Tools                json.RawMessage `json:"tools,omitempty"`
	ToolChoice           json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool           `json:"parallel_tool_calls,omitempty"`
	Temperature          *float64        `json:"temperature,omitempty"`
	MaxOutputTokens      *int            `json:"max_output_tokens,omitempty"`
	Stream               bool            `json:"stream,omitempty"`
	Store                *bool           `json:"store,omitempty"`
	User                 string          `json:"user,omitempty"`
	PromptCacheKey       string          `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string          `json:"prompt_cache_retention,omitempty"`
	Metadata             json.RawMessage `json:"metadata,omitempty"`
	PreviousResponseID   string          `json:"previous_response_id,omitempty"`
}

type storedResponse struct {
	Response map[string]any
	System   []string
	Messages []domain.BedrockChatMessage
	ItemIDs  map[string]struct{}
}

type storedResponseItem struct {
	Messages []domain.BedrockChatMessage
}

type normalizedResponseRequest struct {
	Messages        []domain.BedrockChatMessage
	System          []string
	Tools           []domain.ToolDefinition
	ToolChoice      *domain.ToolChoice
	ToolsResponse   []map[string]any
	ToolChoiceValue any
}

type itemReferenceResolver func(id string) (storedResponseItem, bool)

func normalizeResponseRequest(req ResponseRequest, previous *storedResponse) (*normalizedResponseRequest, error) {
	return normalizeResponseRequestWithResolver(req, previous, nil)
}

func normalizeResponseRequestWithResolver(req ResponseRequest, previous *storedResponse, resolveItem itemReferenceResolver) (*normalizedResponseRequest, error) {
	seenItemReferences := map[string]struct{}{}
	resolveItemForRequest := func(id string) (storedResponseItem, bool) {
		if id == "" {
			return storedResponseItem{}, false
		}
		if _, ok := seenItemReferences[id]; ok {
			return storedResponseItem{}, true
		}
		seenItemReferences[id] = struct{}{}
		if previous != nil {
			if _, ok := previous.ItemIDs[id]; ok {
				return storedResponseItem{}, true
			}
		}
		if resolveItem == nil {
			return storedResponseItem{}, false
		}
		return resolveItem(id)
	}

	finalize := func(messages []domain.BedrockChatMessage, system []string) (*normalizedResponseRequest, error) {
		normalizedMessages, normalizedSystem, err := normalizeBedrockConversation(messages, system)
		if err != nil {
			return nil, err
		}
		tools, toolsResp, err := parseResponseTools(req.Tools)
		if err != nil {
			return nil, err
		}
		choice, choiceValue, err := parseResponseToolChoice(req.ToolChoice, len(tools) > 0)
		if err != nil {
			return nil, err
		}
		return &normalizedResponseRequest{
			Messages:        normalizedMessages,
			System:          normalizedSystem,
			Tools:           tools,
			ToolChoice:      choice,
			ToolsResponse:   toolsResp,
			ToolChoiceValue: choiceValue,
		}, nil
	}

	var messages []domain.BedrockChatMessage
	var system []string
	if previous != nil {
		messages = cloneMessages(previous.Messages)
		system = cloneStrings(previous.System)
	}
	if len(req.Instructions) > 0 && string(req.Instructions) != "null" {
		text, err := messageText(req.Instructions)
		if err != nil {
			return nil, fmt.Errorf("unsupported instructions shape")
		}
		if strings.TrimSpace(text) != "" {
			system = append(system, text)
		}
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		if len(messages) == 0 && len(system) == 0 {
			return nil, fmt.Errorf("input is required")
		}
		return finalize(messages, system)
	}

	var direct string
	if err := json.Unmarshal(req.Input, &direct); err == nil {
		appendResponseMessage(&messages, "user", domain.BedrockContentBlock{Type: "text", Text: direct})
		return finalize(messages, system)
	}

	var single json.RawMessage
	if err := json.Unmarshal(req.Input, &single); err == nil && len(single) > 0 && single[0] == '{' {
		msgs, sys, err := parseResponseInputItem(single, resolveItemForRequest)
		if err != nil {
			return nil, err
		}
		messages = mergeResponseMessages(messages, msgs)
		system = append(system, sys...)
		return finalize(messages, system)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, fmt.Errorf("unsupported input shape")
	}
	for _, item := range items {
		msgs, sys, err := parseResponseInputItem(item, resolveItemForRequest)
		if err != nil {
			return nil, err
		}
		messages = mergeResponseMessages(messages, msgs)
		system = append(system, sys...)
	}
	return finalize(messages, system)
}

func normalizeBedrockConversation(messages []domain.BedrockChatMessage, system []string) ([]domain.BedrockChatMessage, []string, error) {
	for len(messages) > 0 && messages[0].Role == "assistant" {
		if !assistantMessageCanBecomeSystem(messages[0]) {
			return nil, nil, fmt.Errorf("conversation must start with a user message")
		}
		text := strings.TrimSpace(assistantMessageText(messages[0]))
		if text != "" {
			system = append(system, "Previous assistant message:\n"+text)
		}
		messages = messages[1:]
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("conversation must include at least one user message")
	}
	if messages[0].Role != "user" {
		return nil, nil, fmt.Errorf("conversation must start with a user message")
	}
	messages = mergeAdjacentSameRoleMessages(messages)
	messages = normalizeToolResultAdjacency(messages)
	if err := validateToolResultAdjacency(messages); err != nil {
		return nil, nil, err
	}
	return messages, system, nil
}

func normalizeToolResultAdjacency(messages []domain.BedrockChatMessage) []domain.BedrockChatMessage {
	messages = mergeAdjacentSameRoleMessages(messages)
	for i := 0; i < len(messages); i++ {
		required := toolUseIDs(messages[i].Blocks)
		if len(required) == 0 {
			continue
		}
		if i == len(messages)-1 || messages[i+1].Role != "user" {
			messages = insertUserMessageAfter(messages, i)
		}
		messages, _ = moveLaterToolResultsAfter(messages, i, required)
	}
	return dropEmptyMessages(messages)
}

func mergeAdjacentSameRoleMessages(messages []domain.BedrockChatMessage) []domain.BedrockChatMessage {
	merged := make([]domain.BedrockChatMessage, 0, len(messages))
	for _, msg := range messages {
		if len(merged) > 0 && merged[len(merged)-1].Role == msg.Role {
			last := &merged[len(merged)-1]
			last.Blocks = append(last.Blocks, cloneBlocks(msg.Blocks)...)
			if len(last.Blocks) > 0 {
				last.Content = blocksText(last.Blocks)
			} else {
				last.Content += msg.Content
			}
			continue
		}
		merged = append(merged, msg)
	}
	return merged
}

func toolUseIDs(blocks []domain.BedrockContentBlock) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, block := range blocks {
		if block.Type == "tool_use" && block.ToolUseID != "" {
			ids[block.ToolUseID] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

func moveMatchingToolResultsFirst(blocks []domain.BedrockContentBlock, required map[string]struct{}) ([]domain.BedrockContentBlock, bool) {
	firstNonResult := -1
	matchingResults := make([]domain.BedrockContentBlock, 0, len(required))
	remaining := make([]domain.BedrockContentBlock, 0, len(blocks))
	changed := false
	for i, block := range blocks {
		if block.Type == "tool_result" {
			if _, ok := required[block.ToolUseID]; ok {
				matchingResults = append(matchingResults, block)
				if firstNonResult >= 0 {
					changed = true
				}
				continue
			}
		}
		if firstNonResult < 0 {
			firstNonResult = i
		}
		remaining = append(remaining, block)
	}
	if len(matchingResults) == 0 || !changed {
		return blocks, false
	}
	reordered := make([]domain.BedrockContentBlock, 0, len(blocks))
	reordered = append(reordered, matchingResults...)
	reordered = append(reordered, remaining...)
	return reordered, true
}

func insertUserMessageAfter(messages []domain.BedrockChatMessage, index int) []domain.BedrockChatMessage {
	messages = append(messages, domain.BedrockChatMessage{})
	copy(messages[index+2:], messages[index+1:])
	messages[index+1] = domain.BedrockChatMessage{Role: "user"}
	return messages
}

func moveLaterToolResultsAfter(messages []domain.BedrockChatMessage, toolUseIndex int, required map[string]struct{}) ([]domain.BedrockChatMessage, bool) {
	next := &messages[toolUseIndex+1]
	ensureMessageBlocks(next)
	nextBlocks, changed := moveMatchingToolResultsFirst(next.Blocks, required)
	next.Blocks = nextBlocks

	present := toolResultIDs(next.Blocks)
	for id := range required {
		if _, ok := present[id]; ok {
			delete(required, id)
		}
	}
	if len(required) == 0 {
		next.Content = blocksText(next.Blocks)
		return messages, changed
	}

	moved := []domain.BedrockContentBlock{}
	for i := toolUseIndex + 2; i < len(messages); i++ {
		if messages[i].Role != "user" {
			continue
		}
		ensureMessageBlocks(&messages[i])
		blocks, extracted := extractMatchingToolResults(messages[i].Blocks, required)
		if len(extracted) == 0 {
			continue
		}
		moved = append(moved, extracted...)
		messages[i].Blocks = blocks
		messages[i].Content = blocksText(blocks)
		for _, block := range extracted {
			delete(required, block.ToolUseID)
		}
		changed = true
		if len(required) == 0 {
			break
		}
	}
	if len(moved) > 0 {
		next.Blocks = append(moved, next.Blocks...)
		next.Content = blocksText(next.Blocks)
	}
	return messages, changed
}

func ensureMessageBlocks(message *domain.BedrockChatMessage) {
	if len(message.Blocks) == 0 && strings.TrimSpace(message.Content) != "" {
		message.Blocks = []domain.BedrockContentBlock{{
			Type: "text",
			Text: message.Content,
		}}
	}
}

func dropEmptyMessages(messages []domain.BedrockChatMessage) []domain.BedrockChatMessage {
	kept := make([]domain.BedrockChatMessage, 0, len(messages))
	for _, msg := range messages {
		if len(msg.Blocks) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue
		}
		kept = append(kept, msg)
	}
	return kept
}

func extractMatchingToolResults(blocks []domain.BedrockContentBlock, required map[string]struct{}) ([]domain.BedrockContentBlock, []domain.BedrockContentBlock) {
	remaining := make([]domain.BedrockContentBlock, 0, len(blocks))
	moved := []domain.BedrockContentBlock{}
	for _, block := range blocks {
		if block.Type == "tool_result" {
			if _, ok := required[block.ToolUseID]; ok {
				moved = append(moved, block)
				continue
			}
		}
		remaining = append(remaining, block)
	}
	return remaining, moved
}

func toolResultIDs(blocks []domain.BedrockContentBlock) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, block := range blocks {
		if block.Type == "tool_result" && block.ToolUseID != "" {
			ids[block.ToolUseID] = struct{}{}
		}
	}
	return ids
}

func validateToolResultAdjacency(messages []domain.BedrockChatMessage) error {
	for i, msg := range messages {
		required := toolUseIDs(msg.Blocks)
		if len(required) == 0 {
			continue
		}
		if i == len(messages)-1 || messages[i+1].Role != "user" {
			return fmt.Errorf("tool_use ids require a following user tool_result message: %s", strings.Join(sortedMapKeys(required), ", "))
		}
		missing := copyStringSet(required)
		for _, block := range messages[i+1].Blocks {
			if block.Type != "tool_result" {
				break
			}
			delete(missing, block.ToolUseID)
		}
		if len(missing) > 0 {
			return fmt.Errorf("tool_use ids require matching tool_result blocks first in the next user message: %s", strings.Join(sortedMapKeys(missing), ", "))
		}
	}
	return nil
}

func copyStringSet(values map[string]struct{}) map[string]struct{} {
	copied := make(map[string]struct{}, len(values))
	for key := range values {
		copied[key] = struct{}{}
	}
	return copied
}

func sortedMapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assistantMessageCanBecomeSystem(message domain.BedrockChatMessage) bool {
	if len(message.Blocks) == 0 {
		return true
	}
	for _, block := range message.Blocks {
		if block.Type != "" && block.Type != "text" {
			return false
		}
	}
	return true
}

func assistantMessageText(message domain.BedrockChatMessage) string {
	if len(message.Blocks) > 0 {
		return blocksText(message.Blocks)
	}
	return message.Content
}

func parseResponseInputItem(raw json.RawMessage, resolveItem itemReferenceResolver) ([]domain.BedrockChatMessage, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return []domain.BedrockChatMessage{{Role: "user", Content: direct}}, nil, nil
	}

	var item struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		CallID    string          `json:"call_id"`
		Output    json.RawMessage `json:"output"`
		Status    string          `json:"status"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, nil, fmt.Errorf("unsupported input item shape")
	}
	switch item.Type {
	case "item_reference":
		if resolveItem == nil {
			return nil, nil, nil
		}
		resolved, ok := resolveItem(item.ID)
		if !ok {
			return nil, nil, nil
		}
		return cloneMessages(resolved.Messages), nil, nil
	case "", "message":
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role == "" {
			role = "user"
		}
		blocks, err := parseResponseMessageContent(item.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("unsupported message content shape")
		}
		if role == "system" || role == "developer" {
			return nil, []string{blocksText(blocks)}, nil
		}
		if role != "user" && role != "assistant" {
			return nil, nil, fmt.Errorf("unsupported message role %q", item.Role)
		}
		msgs := []domain.BedrockChatMessage{}
		appendResponseMessage(&msgs, role, blocks...)
		return msgs, nil, nil
	case "input_text", "output_text", "text":
		msgs := []domain.BedrockChatMessage{}
		appendResponseMessage(&msgs, "user", domain.BedrockContentBlock{Type: "text", Text: item.Text})
		return msgs, nil, nil
	case "function_call":
		msgs := []domain.BedrockChatMessage{}
		appendResponseMessage(&msgs, "assistant", domain.BedrockContentBlock{
			Type:      "tool_use",
			ToolUseID: item.CallID,
			ToolName:  item.Name,
			ToolInput: []byte(item.Arguments),
		})
		return msgs, nil, nil
	case "function_call_output":
		result, err := parseFunctionCallOutput(item.Output)
		if err != nil {
			return nil, nil, err
		}
		status := "success"
		if item.Status == "error" {
			status = "error"
		}
		msgs := []domain.BedrockChatMessage{}
		appendResponseMessage(&msgs, "user", domain.BedrockContentBlock{
			Type:             "tool_result",
			ToolUseID:        item.CallID,
			ToolResultStatus: status,
			ToolResult:       result,
		})
		return msgs, nil, nil
	case "reasoning":
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported input item type %q", item.Type)
	}
}

func buildResponseEnvelope(id string, req ResponseRequest, normalized *normalizedResponseRequest, upstreamResp *domain.ConverseResponse) map[string]any {
	createdAt := time.Now().Unix()
	output, parallelToolCalls := buildResponseOutputItems(upstreamResp)
	response := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          createdAt,
		"status":              "completed",
		"error":               nil,
		"incomplete_details":  nil,
		"model":               req.Model,
		"output":              output,
		"parallel_tool_calls": parallelToolCalls,
		"tools":               normalized.ToolsResponse,
		"tool_choice":         normalized.ToolChoiceValue,
		"truncation":          "disabled",
		"usage": map[string]any{
			"input_tokens": upstreamResp.Usage.Input,
			"input_tokens_details": map[string]any{
				"cached_tokens": upstreamResp.Usage.CacheRead,
			},
			"output_tokens": upstreamResp.Usage.Output,
			"output_tokens_details": map[string]any{
				"reasoning_tokens": 0,
			},
			"total_tokens": upstreamResp.Usage.Total,
		},
		"text": map[string]any{
			"format": map[string]any{
				"type": "text",
			},
		},
	}
	if req.MaxOutputTokens != nil {
		response["max_output_tokens"] = *req.MaxOutputTokens
	} else {
		response["max_output_tokens"] = nil
	}
	if req.Temperature != nil {
		response["temperature"] = *req.Temperature
	} else {
		response["temperature"] = nil
	}
	if req.PreviousResponseID != "" {
		response["previous_response_id"] = req.PreviousResponseID
	} else {
		response["previous_response_id"] = nil
	}
	if len(req.Instructions) > 0 && string(req.Instructions) != "null" {
		if text, err := messageText(req.Instructions); err == nil {
			response["instructions"] = text
		} else {
			response["instructions"] = nil
		}
	} else {
		response["instructions"] = nil
	}
	if req.Store != nil {
		response["store"] = *req.Store
	} else {
		response["store"] = false
	}
	if len(req.Metadata) > 0 && string(req.Metadata) != "null" {
		var metadata map[string]any
		if err := json.Unmarshal(req.Metadata, &metadata); err == nil {
			response["metadata"] = metadata
		}
	}
	if req.User != "" {
		response["user"] = req.User
	} else {
		response["user"] = nil
	}
	if req.PromptCacheKey != "" {
		response["prompt_cache_key"] = req.PromptCacheKey
	}
	if req.PromptCacheRetention != "" {
		response["prompt_cache_retention"] = req.PromptCacheRetention
	}
	response["output_text"] = upstreamResp.Text
	return response
}

func buildResponseOutputItems(resp *domain.ConverseResponse) ([]map[string]any, bool) {
	message := resp.Message
	if len(message.Blocks) == 0 && message.Content == "" && resp.Text != "" {
		message.Content = resp.Text
	}
	if len(message.Blocks) == 0 && message.Content != "" {
		message.Blocks = []domain.BedrockContentBlock{{Type: "text", Text: message.Content}}
	}
	output := []map[string]any{}
	var textParts []string
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		text := strings.Join(textParts, "")
		output = append(output, map[string]any{
			"id":     "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
		textParts = nil
	}
	toolCalls := 0
	for _, block := range message.Blocks {
		switch block.Type {
		case "", "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			flushText()
			output = append(output, map[string]any{
				"id":         "fc_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
				"type":       "function_call",
				"status":     "completed",
				"call_id":    block.ToolUseID,
				"name":       block.ToolName,
				"arguments":  string(nonEmptyJSON(block.ToolInput, []byte(`{}`))),
				"created_by": "model",
			})
			toolCalls++
		}
	}
	flushText()
	return output, toolCalls > 1
}

func responseOutputItems(response map[string]any) (map[string]struct{}, map[string]storedResponseItem) {
	output, _ := response["output"].([]map[string]any)
	ids := make(map[string]struct{}, len(output))
	items := make(map[string]storedResponseItem, len(output))
	for _, item := range output {
		id, _ := item["id"].(string)
		if id != "" {
			ids[id] = struct{}{}
			if messages := outputItemMessages(item); len(messages) > 0 {
				items[id] = storedResponseItem{Messages: messages}
			}
		}
	}
	return ids, items
}

func outputItemMessages(item map[string]any) []domain.BedrockChatMessage {
	switch itemType, _ := item["type"].(string); itemType {
	case "message":
		blocks := []domain.BedrockContentBlock{}
		content, _ := item["content"].([]map[string]any)
		for _, part := range content {
			partType, _ := part["type"].(string)
			if partType != "" && partType != "output_text" && partType != "text" {
				continue
			}
			text, _ := part["text"].(string)
			if text != "" {
				blocks = append(blocks, domain.BedrockContentBlock{Type: "text", Text: text})
			}
		}
		if len(blocks) == 0 {
			return nil
		}
		return []domain.BedrockChatMessage{{
			Role:    "assistant",
			Content: blocksText(blocks),
			Blocks:  blocks,
		}}
	case "function_call":
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		arguments, _ := item["arguments"].(string)
		if callID == "" {
			return nil
		}
		return []domain.BedrockChatMessage{{
			Role: "assistant",
			Blocks: []domain.BedrockContentBlock{{
				Type:      "tool_use",
				ToolUseID: callID,
				ToolName:  name,
				ToolInput: []byte(nonEmptyJSON([]byte(arguments), []byte(`{}`))),
			}},
		}}
	default:
		return nil
	}
}

func parseResponseTools(raw json.RawMessage) ([]domain.ToolDefinition, []map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, []map[string]any{}, nil
	}
	var items []struct {
		Type              string          `json:"type"`
		Name              string          `json:"name"`
		Description       string          `json:"description"`
		Parameters        json.RawMessage `json:"parameters"`
		Strict            *bool           `json:"strict"`
		ExternalWebAccess *bool           `json:"external_web_access"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("invalid tools: %w", err)
	}
	defs := make([]domain.ToolDefinition, 0, len(items))
	echo := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "function":
		case "web_search":
			tool := map[string]any{
				"type": "web_search",
			}
			if item.ExternalWebAccess != nil {
				tool["external_web_access"] = *item.ExternalWebAccess
			}
			echo = append(echo, tool)
			continue
		default:
			return nil, nil, fmt.Errorf("unsupported tool type %q", item.Type)
		}
		defs = append(defs, domain.ToolDefinition{
			Name:        item.Name,
			Description: item.Description,
			Parameters:  nonEmptyJSON(item.Parameters, []byte(`{"type":"object","properties":{}}`)),
			Strict:      item.Strict,
		})
		tool := map[string]any{
			"type":       "function",
			"name":       item.Name,
			"parameters": json.RawMessage(nonEmptyJSON(item.Parameters, []byte(`{"type":"object","properties":{}}`))),
		}
		if item.Description != "" {
			tool["description"] = item.Description
		}
		if item.Strict != nil {
			tool["strict"] = *item.Strict
		}
		echo = append(echo, tool)
	}
	return defs, echo, nil
}

func parseResponseToolChoice(raw json.RawMessage, hasTools bool) (*domain.ToolChoice, any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if !hasTools {
			return nil, "none", nil
		}
		return &domain.ToolChoice{Type: "auto"}, "auto", nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		switch direct {
		case "auto", "required", "none":
			return &domain.ToolChoice{Type: direct}, direct, nil
		default:
			return nil, nil, fmt.Errorf("unsupported tool_choice %q", direct)
		}
	}
	var specific struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &specific); err != nil {
		return nil, nil, fmt.Errorf("invalid tool_choice: %w", err)
	}
	if specific.Type != "function" {
		return nil, nil, fmt.Errorf("unsupported tool_choice type %q", specific.Type)
	}
	return &domain.ToolChoice{Type: "function", Name: specific.Name}, map[string]any{
		"type": "function",
		"name": specific.Name,
	}, nil
}

func parseResponseMessageContent(raw json.RawMessage) ([]domain.BedrockContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return []domain.BedrockContentBlock{{Type: "text", Text: direct}}, nil
	}
	var single struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.Text != "" {
		return []domain.BedrockContentBlock{{Type: "text", Text: single.Text}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	blocks := make([]domain.BedrockContentBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "", "text", "input_text", "output_text":
			blocks = append(blocks, domain.BedrockContentBlock{Type: "text", Text: part.Text})
		}
	}
	return blocks, nil
}

func parseFunctionCallOutput(raw json.RawMessage) ([]domain.ToolResultContent, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return []domain.ToolResultContent{{Type: "text", Text: direct}}, nil
	}
	var jsonValue any
	if err := json.Unmarshal(raw, &jsonValue); err == nil {
		switch jsonValue.(type) {
		case map[string]any, []any, bool, float64:
			return []domain.ToolResultContent{{
				Type: "json",
				JSON: append([]byte(nil), raw...),
			}}, nil
		}
	}
	var parts []struct {
		Type string          `json:"type"`
		Text string          `json:"text"`
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("unsupported function_call_output shape")
	}
	content := make([]domain.ToolResultContent, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "", "input_text", "text":
			content = append(content, domain.ToolResultContent{Type: "text", Text: part.Text})
		case "json":
			content = append(content, domain.ToolResultContent{Type: "json", JSON: append([]byte(nil), nonEmptyJSON(part.JSON, []byte(`null`))...)})
		default:
			return nil, fmt.Errorf("unsupported function_call_output content type %q", part.Type)
		}
	}
	return content, nil
}

func appendResponseMessage(messages *[]domain.BedrockChatMessage, role string, blocks ...domain.BedrockContentBlock) {
	if len(blocks) == 0 {
		return
	}
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == role {
		last := &(*messages)[len(*messages)-1]
		last.Blocks = append(last.Blocks, blocks...)
		last.Content = blocksText(last.Blocks)
		return
	}
	*messages = append(*messages, domain.BedrockChatMessage{
		Role:    role,
		Content: blocksText(blocks),
		Blocks:  cloneBlocks(blocks),
	})
}

func blocksText(blocks []domain.BedrockContentBlock) string {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func cloneMessages(messages []domain.BedrockChatMessage) []domain.BedrockChatMessage {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]domain.BedrockChatMessage, len(messages))
	copy(cloned, messages)
	for i := range cloned {
		cloned[i].Blocks = cloneBlocks(messages[i].Blocks)
	}
	return cloned
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return cloned
}

func cloneBlocks(blocks []domain.BedrockContentBlock) []domain.BedrockContentBlock {
	if len(blocks) == 0 {
		return nil
	}
	cloned := make([]domain.BedrockContentBlock, len(blocks))
	copy(cloned, blocks)
	for i := range cloned {
		cloned[i].ToolInput = append([]byte(nil), blocks[i].ToolInput...)
		if len(blocks[i].ToolResult) > 0 {
			cloned[i].ToolResult = make([]domain.ToolResultContent, len(blocks[i].ToolResult))
			copy(cloned[i].ToolResult, blocks[i].ToolResult)
			for j := range cloned[i].ToolResult {
				cloned[i].ToolResult[j].JSON = append([]byte(nil), blocks[i].ToolResult[j].JSON...)
			}
		}
	}
	return cloned
}

func mergeResponseMessages(existing []domain.BedrockChatMessage, incoming []domain.BedrockChatMessage) []domain.BedrockChatMessage {
	for _, msg := range incoming {
		if len(existing) > 0 && existing[len(existing)-1].Role == msg.Role {
			existing[len(existing)-1].Blocks = append(existing[len(existing)-1].Blocks, cloneBlocks(msg.Blocks)...)
			existing[len(existing)-1].Content = blocksText(existing[len(existing)-1].Blocks)
			continue
		}
		existing = append(existing, msg)
	}
	return existing
}

func nonEmptyJSON(raw []byte, fallback []byte) []byte {
	if len(raw) == 0 {
		return fallback
	}
	return raw
}
