package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

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

// Regex to match <tool_code>...</tool_code> blocks
var toolCodeRegex = regexp.MustCompile(`(?s)<tool_code>([\s\S]*?)</tool_code>`)

// parseToolCallOutput examines the raw text output from qodercli and extracts any tool calls
func parseToolCallOutput(text string) *ParsedToolOutput {
	matches := toolCodeRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return &ParsedToolOutput{Type: "text", PrefixText: text}
	}

	prefixText := text
	firstMatchIndex := strings.Index(text, "<tool_code>")
	if firstMatchIndex > 0 {
		prefixText = strings.TrimSpace(text[:firstMatchIndex])
	} else {
		prefixText = ""
	}

	var toolCalls []ToolCall
	for i, match := range matches {
		if len(match) < 2 {
			continue
		}

		toolJson := strings.TrimSpace(match[1])
		var toolData map[string]interface{}

		if err := json.Unmarshal([]byte(toolJson), &toolData); err != nil {
			// Try to recover basic action
			if strings.Contains(toolJson, `"action":`) {
				continue // Skip invalid JSON tools
			}
		}

		name, _ := toolData["action"].(string)
		if name == "" {
			name = "unknown_tool"
		}

		argsStr := "{}"
		if argsMap, ok := toolData["args"].(map[string]interface{}); ok {
			b, _ := json.Marshal(argsMap)
			argsStr = string(b)
		} else if argStr, ok := toolData["args"].(string); ok {
			argsStr = argStr
		}

		toolCalls = append(toolCalls, ToolCall{
			ID:   fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i),
			Type: "function",
			Function: ToolCallFunction{
				Name:      name,
				Arguments: argsStr,
			},
		})
	}

	if len(toolCalls) > 0 {
		return &ParsedToolOutput{
			Type:       "tool_calls",
			PrefixText: prefixText,
			ToolCalls:  toolCalls,
		}
	}

	return &ParsedToolOutput{Type: "text", PrefixText: text}
}

func executeToolCall(call ToolCall) interface{} {
	// Dummy execution layer - in reality this would execute local commands.
	// For proxying, we just log it and return a mocked response indicating proxy completion
	return map[string]interface{}{
		"status":  "success",
		"message": fmt.Sprintf("Proxy executed tool %s successfully (mocked)", call.Function.Name),
		"output":  "Command executed.",
	}
}
