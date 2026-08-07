package main

import (
	"encoding/json"
	"testing"
)

// The Responses API sends tool fields flat; the CN gateway only accepts the
// nested chat-completions shape and answers 400 otherwise.
func TestNormalizeOpenAIToolsNestsFlatFunctionTools(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]`)

	var got []map[string]interface{}
	if err := json.Unmarshal(normalizeOpenAITools(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	fn, ok := got[0]["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("no nested function object: %+v", got[0])
	}
	if fn["name"] != "read" || fn["description"] != "Read a file" {
		t.Fatalf("function = %+v", fn)
	}
	if _, ok := fn["parameters"].(map[string]interface{}); !ok {
		t.Fatalf("parameters not carried over: %+v", fn)
	}
	if _, leaked := got[0]["name"]; leaked {
		t.Fatalf("flat name still present at top level: %+v", got[0])
	}
}

func TestNormalizeOpenAIToolsLeavesNestedUntouched(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","function":{"name":"write","parameters":{"type":"object"}}}]`)

	var got []map[string]interface{}
	if err := json.Unmarshal(normalizeOpenAITools(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fn, ok := got[0]["function"].(map[string]interface{})
	if !ok || fn["name"] != "write" {
		t.Fatalf("nested tool mangled: %+v", got[0])
	}
}

func TestNormalizeOpenAIToolsPassesThroughHostedTools(t *testing.T) {
	raw := json.RawMessage(`[{"type":"web_search_preview"}]`)

	var got []map[string]interface{}
	if err := json.Unmarshal(normalizeOpenAITools(raw), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got[0]["type"] != "web_search_preview" {
		t.Fatalf("hosted tool mangled: %+v", got[0])
	}
	if _, wrapped := got[0]["function"]; wrapped {
		t.Fatalf("hosted tool should not be wrapped: %+v", got[0])
	}
}

func TestResponsesRequestToChatRequestNormalizesTools(t *testing.T) {
	req := ResponsesRequest{
		Model: "glm-5.2",
		Input: "hi",
		Tools: json.RawMessage(`[{"type":"function","name":"grep","parameters":{"type":"object"}}]`),
	}

	chatReq := responsesRequestToChatRequest(req, "")

	var got []map[string]interface{}
	if err := json.Unmarshal(chatReq.Tools, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fn, ok := got[0]["function"].(map[string]interface{})
	if !ok || fn["name"] != "grep" {
		t.Fatalf("tools reached ChatRequest un-normalized: %+v", got[0])
	}
}
