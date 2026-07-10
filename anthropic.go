package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Anthropic Request Structures
type AnthropicRequest struct {
	Model    string             `json:"model"`
	Messages []AnthropicMessage `json:"messages"`
	// System accepts either a plain string or an array of text blocks (the
	// cache_control-annotated shape some SDKs send) — normalizeAnthropicContent
	// flattens either into plain text.
	System          interface{}     `json:"system,omitempty"`
	MaxTokens       int             `json:"max_tokens"`
	Stream          bool            `json:"stream"`
	Temperature     *float64        `json:"temperature,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Tools           json.RawMessage `json:"tools,omitempty"`
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
	Type string `json:"type"` // "text" | "thinking" | "tool_use"
	Text string `json:"text,omitempty"`
	// Thinking carries the model's reasoning text for type "thinking" blocks.
	Thinking string `json:"thinking,omitempty"`
	// tool_use fields — Input is a parsed object/array/scalar, never a JSON
	// string (Anthropic spec differs from OpenAI's stringified `arguments`).
	ID    string      `json:"id,omitempty"`
	Name  string      `json:"name,omitempty"`
	Input interface{} `json:"input,omitempty"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
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
	Type         string          `json:"type"` // "text_delta" | "thinking_delta" | "input_json_delta"
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	PartialJSON  string          `json:"partial_json,omitempty"`
	StopReason   string          `json:"stop_reason,omitempty"`
	StopSequence string          `json:"stop_sequence,omitempty"`
	Usage        *AnthropicUsage `json:"usage,omitempty"`
}

// Conversion Helpers

// normalizeAnthropicContent flattens an Anthropic message `content` field —
// a plain string or an array of typed blocks (text/tool_result/tool_use/
// image/thinking/document) — into a single text blob the CLI can read. Tool
// round-trips are preserved as <tool_use>/<tool_result> tagged text so the
// model can see its own prior calls and their results in history.
func normalizeAnthropicContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]interface{})
	if !ok {
		return fmt.Sprintf("%v", content)
	}

	var parts []string
	for _, raw := range arr {
		if s, ok := raw.(string); ok {
			if s != "" {
				parts = append(parts, s)
			}
			continue
		}
		part, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		switch partType {
		case "text":
			if t, ok := part["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		case "tool_result":
			toolUseID, _ := part["tool_use_id"].(string)
			toolText := normalizeAnthropicContent(part["content"])
			if toolText != "" {
				parts = append(parts, fmt.Sprintf("<tool_result id=\"%s\">\n%s\n</tool_result>", toolUseID, toolText))
			}
		case "tool_use":
			name, _ := part["name"].(string)
			id, _ := part["id"].(string)
			input := part["input"]
			if input == nil {
				input = map[string]interface{}{}
			}
			inputJSON, err := json.Marshal(input)
			if err != nil {
				inputJSON = []byte("{}")
			}
			parts = append(parts, fmt.Sprintf("<tool_use name=\"%s\" id=\"%s\">\n%s\n</tool_use>", name, id, string(inputJSON)))
		case "image":
			mediaType := "unknown"
			if src, ok := part["source"].(map[string]interface{}); ok {
				if mt, ok := src["media_type"].(string); ok && mt != "" {
					mediaType = mt
				}
			}
			parts = append(parts, fmt.Sprintf("[image: %s]", mediaType))
		case "thinking":
			if t, ok := part["thinking"].(string); ok && t != "" {
				parts = append(parts, "[thinking]\n"+t)
			}
		case "document":
			label, _ := part["name"].(string)
			if label == "" {
				if src, ok := part["source"].(map[string]interface{}); ok {
					if mt, ok := src["media_type"].(string); ok {
						label = mt
					}
				}
			}
			if label == "" {
				label = "file"
			}
			parts = append(parts, fmt.Sprintf("[document: %s]", label))
		case "":
			if t, ok := part["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		default:
			parts = append(parts, fmt.Sprintf("[unsupported content: %s]", partType))
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeAnthropicSystem flattens the request-level `system` field, which
// may be a plain string or an array of text blocks.
func normalizeAnthropicSystem(system interface{}) string {
	return normalizeAnthropicContent(system)
}

func anthropicMessagesToPrompt(req AnthropicRequest) string {
	var sb strings.Builder

	for _, m := range req.Messages {
		content := normalizeAnthropicContent(m.Content)
		if content != "" {
			sb.WriteString(fmt.Sprintf("%s: %s\n\n", strings.Title(m.Role), content))
		}
	}
	return sb.String()
}

// buildAnthropicResponse builds the non-streaming Anthropic message body,
// switching to tool_use content blocks when parsed is a tool-calls payload.
// inputTokens is the caller-computed token count for the request's system+
// messages; output tokens are derived here from the actual content. When
// thinking is non-empty, a "thinking" block is prepended to Content — it
// always leads in the Anthropic protocol.
func buildAnthropicResponse(id, model, content string, parsed *ParsedToolOutput, inputTokens int, thinking string) AnthropicResponse {
	if parsed != nil && parsed.Type == "tool_calls" {
		var blocks []AnthropicContent
		if thinking != "" {
			blocks = append(blocks, AnthropicContent{Type: "thinking", Thinking: thinking})
		}
		if parsed.PrefixText != "" {
			blocks = append(blocks, AnthropicContent{Type: "text", Text: parsed.PrefixText})
		}
		for _, tc := range parsed.ToolCalls {
			var input interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				input = map[string]interface{}{}
			}
			blocks = append(blocks, AnthropicContent{
				Type:  "tool_use",
				ID:    generateCallId("toolu_"),
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		return AnthropicResponse{
			ID:         id,
			Type:       "message",
			Role:       "assistant",
			Model:      model,
			Content:    blocks,
			StopReason: "tool_use",
			Usage:      AnthropicUsage{InputTokens: inputTokens, OutputTokens: countTokens(thinking + content)},
		}
	}
	return buildAnthropicFullResponse(id, model, content, inputTokens, thinking)
}

func buildAnthropicFullResponse(id string, model string, content string, inputTokens int, thinking string) AnthropicResponse {
	var blocks []AnthropicContent
	if thinking != "" {
		blocks = append(blocks, AnthropicContent{Type: "thinking", Thinking: thinking})
	}
	blocks = append(blocks, AnthropicContent{Type: "text", Text: content})
	return AnthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    blocks,
		StopReason: "end_turn",
		Usage: AnthropicUsage{
			InputTokens:  inputTokens,
			OutputTokens: countTokens(thinking + content),
		},
	}
}

// anthropicRequestToChatRequest converts an Anthropic-protocol request into
// the internal ChatRequest shape so it can flow through the existing CN
// gateway pipeline (buildCNRequestBody/sendCNRequest) unchanged. Tool
// round-trips (tool_use/tool_result blocks) are flattened into
// <tool_use>/<tool_result> tagged text via normalizeAnthropicContent — the
// same prompt-injection representation the CLI path already uses — rather
// than mapped to the gateway's native tool-calling fields.
func anthropicRequestToChatRequest(req AnthropicRequest, toolPrompt string) ChatRequest {
	fullSystem := mergeSystemPrompt(normalizeAnthropicSystem(req.System), toolPrompt)

	var messages []Message
	if fullSystem != "" {
		messages = append(messages, Message{Role: "system", Content: fullSystem})
	}
	for _, m := range req.Messages {
		messages = append(messages, Message{Role: m.Role, Content: normalizeAnthropicContent(m.Content)})
	}

	return ChatRequest{
		Model:           req.Model,
		Messages:        messages,
		Stream:          req.Stream,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
	}
}

func buildAnthropicStartEvent(id string, model string, inputTokens int) AnthropicSSEEvent {
	return AnthropicSSEEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:    id,
			Type:  "message",
			Role:  "assistant",
			Model: model,
			Usage: AnthropicUsage{InputTokens: inputTokens, OutputTokens: 0},
		},
	}
}
