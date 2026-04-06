package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/personal/broxy/internal/domain"
)

type ResponseRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input,omitempty"`
	Instructions       json.RawMessage `json:"instructions,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	MaxOutputTokens    *int            `json:"max_output_tokens,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	User               string          `json:"user,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

type storedResponse struct {
	Response map[string]any
	System   []string
	Messages []domain.BedrockChatMessage
}

func normalizeResponseInput(req ResponseRequest, previous *storedResponse) ([]domain.BedrockChatMessage, []string, error) {
	var messages []domain.BedrockChatMessage
	var system []string
	if previous != nil {
		messages = cloneMessages(previous.Messages)
		system = cloneStrings(previous.System)
	}
	if len(req.Instructions) > 0 && string(req.Instructions) != "null" {
		text, err := messageText(req.Instructions)
		if err != nil {
			return nil, nil, fmt.Errorf("unsupported instructions shape")
		}
		if strings.TrimSpace(text) != "" {
			system = append(system, text)
		}
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		if len(messages) == 0 && len(system) == 0 {
			return nil, nil, fmt.Errorf("input is required")
		}
		return messages, system, nil
	}

	var direct string
	if err := json.Unmarshal(req.Input, &direct); err == nil {
		messages = append(messages, domain.BedrockChatMessage{Role: "user", Content: direct})
		return messages, system, nil
	}

	var single json.RawMessage
	if err := json.Unmarshal(req.Input, &single); err == nil && len(single) > 0 && single[0] == '{' {
		msgs, sys, err := parseResponseInputItem(single)
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, msgs...)
		system = append(system, sys...)
		return messages, system, nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, nil, fmt.Errorf("unsupported input shape")
	}
	for _, item := range items {
		msgs, sys, err := parseResponseInputItem(item)
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, msgs...)
		system = append(system, sys...)
	}
	return messages, system, nil
}

func parseResponseInputItem(raw json.RawMessage) ([]domain.BedrockChatMessage, []string, error) {
	if len(raw) == 0 {
		return nil, nil, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return []domain.BedrockChatMessage{{Role: "user", Content: direct}}, nil, nil
	}

	var item struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Text    string          `json:"text"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, nil, fmt.Errorf("unsupported input item shape")
	}
	switch item.Type {
	case "", "message":
		role := strings.ToLower(strings.TrimSpace(item.Role))
		if role == "" {
			role = "user"
		}
		text, err := messageText(item.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("unsupported message content shape")
		}
		if role == "system" {
			return nil, []string{text}, nil
		}
		if role != "user" && role != "assistant" {
			return nil, nil, fmt.Errorf("unsupported message role %q", item.Role)
		}
		return []domain.BedrockChatMessage{{Role: role, Content: text}}, nil, nil
	case "input_text", "output_text", "text":
		return []domain.BedrockChatMessage{{Role: "user", Content: item.Text}}, nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported input item type %q", item.Type)
	}
}

func buildResponseEnvelope(id string, req ResponseRequest, text string, usage domain.TokenUsage) map[string]any {
	createdAt := time.Now().Unix()
	messageID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	output := []map[string]any{{
		"id":     messageID,
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []map[string]any{{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}}
	response := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          createdAt,
		"status":              "completed",
		"error":               nil,
		"incomplete_details":  nil,
		"model":               req.Model,
		"output":              output,
		"parallel_tool_calls": false,
		"tools":               []any{},
		"tool_choice":         "none",
		"truncation":          "disabled",
		"usage": map[string]any{
			"input_tokens":  usage.Input,
			"output_tokens": usage.Output,
			"total_tokens":  usage.Total,
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
	}
	return response
}

func cloneMessages(messages []domain.BedrockChatMessage) []domain.BedrockChatMessage {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]domain.BedrockChatMessage, len(messages))
	copy(cloned, messages)
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
