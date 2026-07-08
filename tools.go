package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ── Tool call protocol (anti-pollution) ─────────────────────────────────────
//
// The CLI itself has its own tool execution disabled via `--tools ""` and its
// system prompt overridden via `--system-prompt`.  The proxy's job is purely
// protocol translation: inject a *format-only* system prompt so the model
// knows which tools exist and what JSON shape to emit, then parse that JSON
// back into the wire format the caller expects (OpenAI `tool_calls` or
// Anthropic `tool_use` blocks).
//
// The injected prompt must NEVER contain role-defining statements
// ("你是一个…") — that would pollute character agents' personalities.

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ParsedToolOutput struct {
	Type       string
	PrefixText string
	ToolCalls  []ToolCall
}

// normalizedTool is the internal shape used to build the system prompt.
type normalizedTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// ── System-prompt injection ─────────────────────────────────────────────────

// buildToolSystemPrompt builds a format-only system prompt that teaches the
// model the available tools and the JSON code-block shape to emit when it
// wants to call one.  Returns "" when no tools are present.
func buildToolSystemPrompt(tools []normalizedTool) string {
	if len(tools) == 0 {
		return ""
	}
	return "[Tool Protocol] 以下工具可供调用：\n\n" +
		mustJSON(tools) +
		"\n\n如需调用工具，请仅输出以下格式的 JSON 代码块：\n\n```json\n" +
		`{"tool_calls": [{"name": "工具名称", "arguments": {参数对象}}]}` + "\n```\n\n" +
		"如不需要调用工具，直接以正常文本回复，不要输出任何 JSON 代码块。\n" +
		"不要在同一个回复中既输出普通文本又输出工具调用 JSON。"
}

// buildToolSystemPromptFromRaw is a convenience that accepts either OpenAI or
// Anthropic tool definitions in raw JSON form.
func buildToolSystemPromptFromRaw(raw json.RawMessage, anthropic bool) string {
	if len(raw) == 0 {
		return ""
	}
	var tools []normalizedTool
	if anthropic {
		var arr []map[string]interface{}
		if err := json.Unmarshal(raw, &arr); err != nil {
			return ""
		}
		for _, t := range arr {
			name, _ := t["name"].(string)
			desc, _ := t["description"].(string)
			params, _ := t["input_schema"].(map[string]interface{})
			if params == nil {
				params, _ = t["parameters"].(map[string]interface{})
			}
			tools = append(tools, normalizedTool{Name: name, Description: desc, Parameters: params})
		}
	} else {
		var arr []map[string]interface{}
		if err := json.Unmarshal(raw, &arr); err != nil {
			return ""
		}
		for _, t := range arr {
			fn, _ := t["function"].(map[string]interface{})
			if fn == nil {
				fn = t
			}
			name, _ := fn["name"].(string)
			desc, _ := fn["description"].(string)
			params, _ := fn["parameters"].(map[string]interface{})
			tools = append(tools, normalizedTool{Name: name, Description: desc, Parameters: params})
		}
	}
	return buildToolSystemPrompt(tools)
}

// ── Tool result formatting (history round-trips) ────────────────────────────

// formatToolResultForPrompt converts OpenAI `role: tool` messages and
// Anthropic `tool_result` content blocks into the `<tool_result id="...">`
// text blocks the model can read back from its own history.
func formatToolResultForPrompt(toolResults []map[string]interface{}) string {
	if len(toolResults) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range toolResults {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		id, _ := r["tool_call_id"].(string)
		if id == "" {
			id, _ = r["tool_use_id"].(string)
		}
		if id == "" {
			id = "unknown"
		}
		sb.WriteString("<tool_result id=\"" + id + "\">\n")
		switch c := r["content"].(type) {
		case string:
			sb.WriteString(c)
		case []interface{}:
			for _, part := range c {
				if m, ok := part.(map[string]interface{}); ok {
					if t, ok := m["text"].(string); ok {
						sb.WriteString(t)
					}
				} else if s, ok := part.(string); ok {
					sb.WriteString(s)
				}
			}
		default:
			if c != nil {
				sb.WriteString(fmt.Sprintf("%v", c))
			}
		}
		sb.WriteString("\n</tool_result>")
	}
	return sb.String()
}

// ── CLI output parser ───────────────────────────────────────────────────────

// parseToolCallOutput examines the raw text output from qodercli and extracts
// any tool calls.  Two shapes are supported:
//
//  1. A fenced ```json ... ``` block whose body is `{"tool_calls": [...]}`.
//  2. A bare balanced JSON object containing "tool_calls" (model forgot the
//     markdown fences) — found via brace-counting, not a lazy regex.
//
// Returns type="text" when no valid tool_calls are found.
func parseToolCallOutput(text string) *ParsedToolOutput {
	if strings.TrimSpace(text) == "" {
		return &ParsedToolOutput{Type: "text", PrefixText: text}
	}

	jsonString, prefixText := extractToolCallJSON(text)
	if jsonString == "" {
		return &ParsedToolOutput{Type: "text", PrefixText: text}
	}

	parsed, ok := parseToolCallsPayload(jsonString)
	if !ok {
		return &ParsedToolOutput{Type: "text", PrefixText: text}
	}

	return &ParsedToolOutput{
		Type:       "tool_calls",
		PrefixText: prefixText,
		ToolCalls:  parsed,
	}
}

// extractToolCallJSON returns the raw JSON string containing "tool_calls" and
// the text that appeared before it (the model's explanatory prefix).
func extractToolCallJSON(text string) (jsonStr, prefixText string) {
	// Path 1 — fenced ```json block.
	if idx := strings.Index(text, "```json"); idx >= 0 {
		rest := text[idx+len("```json"):]
		end := strings.Index(rest, "```")
		if end >= 0 {
			candidate := strings.TrimSpace(rest[:end])
			if strings.Contains(candidate, `"tool_calls"`) {
				return candidate, strings.TrimSpace(text[:idx])
			}
		}
	}

	// Path 2 — balanced-brace scan for the outermost {…} containing "tool_calls".
	for start := 0; start < len(text); start++ {
		if text[start] != '{' {
			continue
		}
		depth := 0
		inString := false
		escapeNext := false
		end := -1
		for i := start; i < len(text); i++ {
			ch := text[i]
			if escapeNext {
				escapeNext = false
				continue
			}
			if ch == '\\' && inString {
				escapeNext = true
				continue
			}
			if ch == '"' {
				inString = !inString
				continue
			}
			if inString {
				continue
			}
			if ch == '{' {
				depth++
			}
			if ch == '}' {
				depth--
				if depth == 0 {
					end = i
					break
				}
			}
		}
		if end < 0 {
			continue
		}
		candidate := text[start : end+1]
		if !strings.Contains(candidate, `"tool_calls"`) {
			continue
		}
		if json.Valid([]byte(candidate)) {
			return candidate, strings.TrimSpace(text[:start])
		}
	}
	return "", ""
}

// parseToolCallsPayload validates and normalises a `{"tool_calls": [...]}`
// payload into ToolCall structs.
func parseToolCallsPayload(jsonStr string) ([]ToolCall, bool) {
	var wrapper struct {
		ToolCalls []map[string]interface{} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		// Last-ditch: trim to first { … last } and retry.
		first := strings.Index(jsonStr, "{")
		last := strings.LastIndex(jsonStr, "}")
		if first < 0 || last <= first {
			return nil, false
		}
		if err := json.Unmarshal([]byte(jsonStr[first:last+1]), &wrapper); err != nil {
			return nil, false
		}
	}
	if len(wrapper.ToolCalls) == 0 {
		return nil, false
	}
	var calls []ToolCall
	now := time.Now().UnixNano()
	for i, c := range wrapper.ToolCalls {
		name, _ := c["name"].(string)
		if name == "" {
			continue
		}
		args := c["arguments"]
		if args == nil {
			args = map[string]interface{}{}
		}
		argsBytes, err := json.Marshal(args)
		if err != nil {
			argsBytes = []byte("{}")
		}
		// Validate that arguments is an object (Anthropic spec) — reject strings.
		var probe map[string]interface{}
		if err := json.Unmarshal(argsBytes, &probe); err != nil {
			continue
		}
		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("call_%d_%d", now, i),
			Type: "function",
			Function: ToolCallFunction{
				Name:      name,
				Arguments: string(argsBytes),
			},
		})
	}
	if len(calls) == 0 {
		return nil, false
	}
	return calls, true
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// generateCallId produces a random call ID with the given prefix.
func generateCallId(prefix string) string {
	buf := make([]byte, 12)
	rand.Read(buf)
	return prefix + hex.EncodeToString(buf)
}
