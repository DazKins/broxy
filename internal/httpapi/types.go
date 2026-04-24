package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/personal/broxy/internal/domain"
)

type ChatCompletionRequest struct {
	Model                string          `json:"model"`
	Messages             []ChatMessage   `json:"messages"`
	Temperature          *float64        `json:"temperature,omitempty"`
	MaxTokens            *int            `json:"max_tokens,omitempty"`
	Stream               bool            `json:"stream,omitempty"`
	User                 string          `json:"user,omitempty"`
	PromptCacheKey       string          `json:"prompt_cache_key,omitempty"`
	PromptCacheRetention string          `json:"prompt_cache_retention,omitempty"`
	Metadata             json.RawMessage `json:"metadata,omitempty"`
}

type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
	Usage   ChatCompletionUsage    `json:"usage"`
}

type ChatCompletionChoice struct {
	Index        int           `json:"index"`
	Message      ChoiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type ChoiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionUsage struct {
	PromptTokens            int                    `json:"prompt_tokens"`
	CompletionTokens        int                    `json:"completion_tokens"`
	TotalTokens             int                    `json:"total_tokens"`
	PromptTokensDetails     ChatPromptTokenDetails `json:"prompt_tokens_details"`
	CompletionTokensDetails ChatCompletionDetails  `json:"completion_tokens_details"`
}

type ChatPromptTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ChatCompletionDetails struct {
	ReasoningTokens          int `json:"reasoning_tokens"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens"`
}

func normalizeMessages(messages []ChatMessage) ([]domain.BedrockChatMessage, []string, error) {
	var chat []domain.BedrockChatMessage
	var system []string
	for _, msg := range messages {
		text, err := messageText(msg.Content)
		if err != nil {
			return nil, nil, err
		}
		switch strings.ToLower(msg.Role) {
		case "system":
			system = append(system, text)
		case "user", "assistant":
			chat = append(chat, domain.BedrockChatMessage{
				Role:    strings.ToLower(msg.Role),
				Content: text,
			})
		default:
			return nil, nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}
	}
	return chat, system, nil
}

func messageText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var single struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &single); err == nil {
		if single.Type == "" || single.Type == "text" || single.Type == "input_text" || single.Type == "output_text" {
			return single.Text, nil
		}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var builder strings.Builder
		for _, part := range parts {
			if part.Type == "" || part.Type == "text" || part.Type == "input_text" || part.Type == "output_text" {
				builder.WriteString(part.Text)
			}
		}
		return builder.String(), nil
	}
	return "", fmt.Errorf("unsupported message content shape")
}
