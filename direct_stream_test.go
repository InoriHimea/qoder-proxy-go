package main

import (
	"strings"
	"testing"
)

func TestClassifyDirectChatChunkConvertsFencedToolCall(t *testing.T) {
	fragments := []string{
		"我將重新詳細分析這個Go語言代理項目的代碼。",
		"讓我從主要文件開始查看。\n\n```",
		"json\n{\"tool",
		"_calls\": [{\"name",
		"\": \"view\",",
		" \"arguments\":",
		" {\"file_path\":",
		" \"main.go\"}}",
		"]}",
		"\n```",
	}

	classifier := &streamClassifier{}
	var text strings.Builder
	var resolved *ParsedToolOutput
	var toolChunks []ChatChunk

	for _, fragment := range fragments {
		chunk := ChatChunk{
			ID:      "chatcmpl-test",
			Object:  "chat.completion.chunk",
			Created: 1,
			Model:   "auto",
			Choices: []ChatChunkChoice{{Index: 0, Delta: ChatChunkDelta{Content: fragment}}},
		}
		chunks, parsed := classifyDirectChatChunk(classifier, chunk, "glm-5.2")
		for _, got := range chunks {
			for _, choice := range got.Choices {
				text.WriteString(choice.Delta.Content)
				if len(choice.Delta.ToolCalls) > 0 {
					toolChunks = append(toolChunks, got)
				}
			}
		}
		if parsed != nil {
			resolved = parsed
		}
	}

	if resolved == nil || len(resolved.ToolCalls) != 1 {
		t.Fatalf("resolved = %+v, want one tool call", resolved)
	}
	if resolved.ToolCalls[0].Function.Name != "view" {
		t.Fatalf("tool name = %q, want view", resolved.ToolCalls[0].Function.Name)
	}
	if resolved.ToolCalls[0].Function.Arguments != `{"file_path":"main.go"}` {
		t.Fatalf("arguments = %q", resolved.ToolCalls[0].Function.Arguments)
	}
	if strings.Contains(text.String(), "tool_calls") || strings.Contains(text.String(), "```json") {
		t.Fatalf("tool JSON leaked into text delta: %q", text.String())
	}
	if len(toolChunks) != 1 {
		t.Fatalf("tool chunk count = %d, want 1", len(toolChunks))
	}
	choice := toolChunks[0].Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choice.FinishReason)
	}
}
