package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The CN gateway emits a trailing `event:finish` payload
// ({"firstTokenDuration":..,"totalDuration":..}) that carries no choices.
// It must not be forwarded as a chat chunk.
func TestParseCNSSELineSkipsFinishTiming(t *testing.T) {
	chunk, done, errMsg, _ := parseCNSSELine(`{"firstTokenDuration":414,"totalDuration":571,"serverDuration":88}`)
	if errMsg != "" || done {
		t.Fatalf("done=%v errMsg=%q, want a plain skip", done, errMsg)
	}
	if chunk != nil {
		t.Fatalf("chunk = %s, want nil for the finish timing line", chunk)
	}
}

func TestParseCNSSELineKeepsRealChunk(t *testing.T) {
	chunk, done, errMsg, _ := parseCNSSELine(`{"choices":[{"delta":{"content":"hi"},"index":0}],"model":"auto"}`)
	if done || errMsg != "" || chunk == nil {
		t.Fatalf("done=%v errMsg=%q chunk=%s, want the chunk forwarded", done, errMsg, chunk)
	}
}

// The gateway always answers with model "auto"; clients need the model they
// asked for echoed back.
func TestClassifyDirectChatChunkRestoresRequestedModel(t *testing.T) {
	classifier := &streamClassifier{}
	chunk := ChatChunk{
		Model:   "auto",
		Choices: []ChatChunkChoice{{Index: 0, Delta: ChatChunkDelta{Content: "hi"}}},
	}
	chunks, _ := classifyDirectChatChunk(classifier, chunk, "glm-5.2")
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0].Model != "glm-5.2" {
		t.Fatalf("model = %q, want glm-5.2", chunks[0].Model)
	}
}

// buildCNRequestBody must round-trip prior tool_calls and tool results, or
// the gateway loses the whole call/result linkage on the follow-up turn.
func TestBuildCNRequestBodyPreservesToolHistory(t *testing.T) {
	req := ChatRequest{
		Model: "glm-5.2",
		Messages: []Message{
			{Role: "user", Content: "Read main.go with view."},
			{Role: "assistant", Content: "", ToolCalls: []ToolCall{{
				ID:       "call_abc123",
				Type:     "function",
				Function: ToolCallFunction{Name: "view", Arguments: `{"file_path":"main.go"}`},
			}}},
			{Role: "tool", ToolCallID: "call_abc123", Content: "package main"},
		},
	}

	bodyJSON, _, _, err := buildCNRequestBody(req, Config{})
	if err != nil {
		t.Fatalf("buildCNRequestBody: %v", err)
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if uerr := json.Unmarshal(bodyJSON, &body); uerr != nil {
		t.Fatalf("unmarshal body: %v", uerr)
	}

	var all strings.Builder
	for _, m := range body.Messages {
		all.WriteString(m.Role + ":" + m.Content + "\n")
	}
	got := all.String()

	for _, want := range []string{"view", "call_abc123", `{"file_path":"main.go"}`, "package main"} {
		if !strings.Contains(got, want) {
			t.Fatalf("body messages missing %q:\n%s", want, got)
		}
	}
	for _, m := range body.Messages {
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" {
			t.Fatalf("assistant tool_calls turn collapsed to empty content:\n%s", got)
		}
	}
}
