package main

import (
	"fmt"
	"strings"
)

// Anthropic Request Structures
type AnthropicRequest struct {
	Model           string             `json:"model"`
	Messages        []AnthropicMessage `json:"messages"`
	System          string             `json:"system,omitempty"`
	MaxTokens       int                `json:"max_tokens"`
	Stream          bool               `json:"stream"`
	Temperature     *float64           `json:"temperature,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
}

type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// Anthropic Response Structures
type AnthropicResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"` // "message"
	Role         string             `json:"role"` // "assistant"
	Content      []AnthropicContent `json:"content"`
	Model        string             `json:"model"`
	StopReason   string             `json:"stop_reason"`
	StopSequence string             `json:"stop_sequence,omitempty"`
	Usage        AnthropicUsage     `json:"usage"`
}

type AnthropicContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Anthropic SSE Event Structures
type AnthropicSSEEvent struct {
	Type         string               `json:"type"`
	Message      *AnthropicResponse   `json:"message,omitempty"`
	Index        int                  `json:"index,omitempty"`
	ContentBlock *AnthropicContent    `json:"content_block,omitempty"`
	Delta        *AnthropicEventDelta `json:"delta,omitempty"`
}

type AnthropicEventDelta struct {
	Type         string          `json:"type"` // "text_delta"
	Text         string          `json:"text,omitempty"`
	StopReason   string          `json:"stop_reason,omitempty"`
	StopSequence string          `json:"stop_sequence,omitempty"`
	Usage        *AnthropicUsage `json:"usage,omitempty"`
}

// Conversion Helpers
func anthropicMessagesToPrompt(req AnthropicRequest) string {
	var sb strings.Builder

	for _, m := range req.Messages {
		content := ""
		switch v := m.Content.(type) {
		case string:
			content = v
		case []interface{}:
			for _, part := range v {
				if p, ok := part.(map[string]interface{}); ok {
					if t, ok := p["text"].(string); ok {
						content += t
					}
				}
			}
		}
		if content != "" {
			sb.WriteString(fmt.Sprintf("%s: %s\n\n", strings.Title(m.Role), content))
		}
	}
	return sb.String()
}

func buildAnthropicFullResponse(id string, model string, content string) AnthropicResponse {
	return AnthropicResponse{
		ID:    id,
		Type:  "message",
		Role:  "assistant",
		Model: model,
		Content: []AnthropicContent{
			{Type: "text", Text: content},
		},
		StopReason: "end_turn",
		Usage: AnthropicUsage{
			InputTokens:  0,
			OutputTokens: 0,
		},
	}
}

func buildAnthropicStartEvent(id string, model string) AnthropicSSEEvent {
	return AnthropicSSEEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:    id,
			Type:  "message",
			Role:  "assistant",
			Model: model,
			Usage: AnthropicUsage{InputTokens: 0, OutputTokens: 0},
		},
	}
}

func buildAnthropicDeltaEvent(text string) AnthropicSSEEvent {
	return AnthropicSSEEvent{
		Type:  "content_block_delta",
		Index: 0,
		Delta: &AnthropicEventDelta{
			Type: "text_delta",
			Text: text,
		},
	}
}
