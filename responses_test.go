package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ── request parsing ─────────────────────────────────────────────────────────

func TestParseResponsesInputString(t *testing.T) {
	msgs := parseResponsesInput("hello world")
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "hello world" {
		t.Fatalf("string input = %+v", msgs)
	}
}

func TestParseResponsesInputItems(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"type": "message", "role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "what's the weather?"},
			},
		},
		map[string]interface{}{
			"type": "function_call", "call_id": "call_1", "name": "get_weather",
			"arguments": `{"city":"NYC"}`,
		},
		map[string]interface{}{
			"type": "function_call_output", "call_id": "call_1", "output": "72F sunny",
		},
	}
	msgs := parseResponsesInput(input)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content != "what's the weather?" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	// function_call must round-trip through the fenced-JSON tool protocol.
	if msgs[1].Role != "assistant" {
		t.Errorf("msg1 role = %q, want assistant", msgs[1].Role)
	}
	parsed := parseToolCallOutput(msgs[1].Content.(string))
	if parsed.Type != "tool_calls" || len(parsed.ToolCalls) != 1 {
		t.Fatalf("function_call did not round-trip: %+v", parsed)
	}
	if parsed.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q", parsed.ToolCalls[0].Function.Name)
	}
	if parsed.ToolCalls[0].Function.Arguments != `{"city":"NYC"}` {
		t.Errorf("tool args = %q", parsed.ToolCalls[0].Function.Arguments)
	}
	// function_call_output must become a readable tool_result block.
	out := msgs[2].Content.(string)
	if !strings.Contains(out, `<tool_result id="call_1">`) || !strings.Contains(out, "72F sunny") {
		t.Errorf("function_call_output = %q", out)
	}
}

func TestResponsesRequestToChatRequestMergesInstructions(t *testing.T) {
	req := ResponsesRequest{
		Model:        "auto",
		Instructions: "You are terse.",
		Input:        "hi",
	}
	chat := responsesRequestToChatRequest(req, "")
	if len(chat.Messages) != 2 {
		t.Fatalf("want system+user, got %+v", chat.Messages)
	}
	if chat.Messages[0].Role != "system" || chat.Messages[0].Content != "You are terse." {
		t.Errorf("system = %+v", chat.Messages[0])
	}
	if chat.Messages[1].Role != "user" || chat.Messages[1].Content != "hi" {
		t.Errorf("user = %+v", chat.Messages[1])
	}
}

func TestResponsesRequestToChatRequestInstructionsPlusToolPrompt(t *testing.T) {
	req := ResponsesRequest{Model: "auto", Instructions: "Base.", Input: "hi"}
	chat := responsesRequestToChatRequest(req, "[Tool Protocol] ...")
	sys, _ := chat.Messages[0].Content.(string)
	if !strings.HasPrefix(sys, "Base.") || !strings.Contains(sys, "[Tool Protocol]") {
		t.Errorf("merged system = %q", sys)
	}
}

func TestResponsesMaxTokensAndEffort(t *testing.T) {
	r := ResponsesRequest{MaxOutputTokens: 500}
	if r.maxTokens() != 500 {
		t.Errorf("max_output_tokens not preferred: %d", r.maxTokens())
	}
	r = ResponsesRequest{MaxTokens: 300}
	if r.maxTokens() != 300 {
		t.Errorf("max_tokens fallback: %d", r.maxTokens())
	}
	r = ResponsesRequest{Reasoning: &ResponsesReasoning{Effort: "high"}}
	if r.reasoningEffort() != "high" {
		t.Errorf("effort = %q", r.reasoningEffort())
	}
}

// Responses top-level tool schema {type,name,description,parameters} must
// normalize through the OpenAI (anthropic=false) branch's fallback.
func TestResponsesToolSchemaNormalizes(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]`)
	prompt := buildToolSystemPromptFromRaw(raw, false)
	if !strings.Contains(prompt, "get_weather") || !strings.Contains(prompt, "city") {
		t.Fatalf("tool prompt missing tool schema: %q", prompt)
	}
}

// ── non-stream builder ──────────────────────────────────────────────────────

func TestBuildResponsesResponseText(t *testing.T) {
	parsed := parseToolCallOutput("just plain text")
	resp := buildResponsesResponse("resp_1", "auto", "just plain text", parsed, 10, "")
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "message" {
		t.Fatalf("output = %+v", resp.Output)
	}
	if resp.Output[0].Content[0].Text != "just plain text" {
		t.Errorf("text = %q", resp.Output[0].Content[0].Text)
	}
}

func TestBuildResponsesResponseToolCalls(t *testing.T) {
	content := "Sure.\n```json\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"NYC\"}}]}\n```"
	parsed := parseToolCallOutput(content)
	resp := buildResponsesResponse("resp_1", "auto", content, parsed, 10, "")
	// Expect a prefix message item + one function_call item.
	var msgItems, fcItems int
	for _, o := range resp.Output {
		switch o.Type {
		case "message":
			msgItems++
		case "function_call":
			fcItems++
			if o.Name != "get_weather" || o.Arguments != `{"city":"NYC"}` {
				t.Errorf("fc = %+v", o)
			}
		}
	}
	if msgItems != 1 || fcItems != 1 {
		t.Fatalf("want 1 message + 1 function_call, got msg=%d fc=%d: %+v", msgItems, fcItems, resp.Output)
	}
}

func TestBuildResponsesResponseReasoning(t *testing.T) {
	resp := buildResponsesResponse("resp_1", "auto", "answer", parseToolCallOutput("answer"), 5, "thinking hard")
	if resp.Output[0].Type != "reasoning" {
		t.Fatalf("reasoning must lead: %+v", resp.Output)
	}
	if resp.Output[0].Summary[0].Text != "thinking hard" {
		t.Errorf("summary = %+v", resp.Output[0].Summary)
	}
}

// ── streaming event sequence ────────────────────────────────────────────────

type sseEvent struct {
	typ string
	seq int
	raw map[string]interface{}
}

func parseResponsesSSE(t *testing.T, s string) []sseEvent {
	t.Helper()
	if strings.Contains(s, "[DONE]") {
		t.Fatalf("Responses stream must NOT contain [DONE]")
	}
	var events []sseEvent
	for _, block := range strings.Split(s, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var evName, dataLine string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				evName = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				dataLine = strings.TrimPrefix(line, "data: ")
			}
		}
		if dataLine == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(dataLine), &m); err != nil {
			t.Fatalf("bad SSE data JSON: %v (%q)", err, dataLine)
		}
		typ, _ := m["type"].(string)
		if typ != evName {
			t.Errorf("event name %q != data type %q", evName, typ)
		}
		seq, _ := m["sequence_number"].(float64)
		events = append(events, sseEvent{typ: typ, seq: int(seq), raw: m})
	}
	return events
}

func assertMonotonicSeq(t *testing.T, events []sseEvent) {
	t.Helper()
	for i, e := range events {
		if e.seq != i {
			t.Errorf("event %d (%s) sequence_number=%d, want %d", i, e.typ, e.seq, i)
		}
	}
}

// driveEmitterText runs the emitter over a plain-text stream and returns the
// captured SSE wire output.
func driveEmitter(fragments []string) string {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	e := newResponsesEmitter(w, "resp_test", "auto", 3)
	e.start()
	classifier := &streamClassifier{}
	var resolved *ParsedToolOutput
	for _, f := range fragments {
		if resolved != nil {
			break
		}
		if r := e.processContent(classifier, f); r != nil {
			resolved = r
		}
	}
	e.finish(classifier, resolved)
	w.Flush()
	return buf.String()
}

func TestResponsesStreamPlainTextSequence(t *testing.T) {
	wire := driveEmitter([]string{"Hello ", "world"})
	events := parseResponsesSSE(t, wire)
	assertMonotonicSeq(t, events)

	var types []string
	for _, e := range events {
		types = append(types, e.typ)
	}
	joined := strings.Join(types, ",")
	for _, must := range []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done",
		"response.completed",
	} {
		if !strings.Contains(joined, must) {
			t.Errorf("missing event %q in sequence: %s", must, joined)
		}
	}
	if types[0] != "response.created" || types[1] != "response.in_progress" {
		t.Errorf("must open with created→in_progress: %s", joined)
	}
	if types[len(types)-1] != "response.completed" {
		t.Errorf("must end with response.completed: %s", joined)
	}
	// Reconstruct the streamed text from deltas.
	var got strings.Builder
	for _, e := range events {
		if e.typ == "response.output_text.delta" {
			got.WriteString(e.raw["delta"].(string))
		}
	}
	if got.String() != "Hello world" {
		t.Errorf("streamed text = %q, want %q", got.String(), "Hello world")
	}
}

func TestResponsesStreamToolCallSequence(t *testing.T) {
	frag := "```json\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"NYC\"}}]}\n```"
	wire := driveEmitter([]string{frag})
	events := parseResponsesSSE(t, wire)
	assertMonotonicSeq(t, events)

	var sawFCAdded, sawFCArgsDone, sawCompleted bool
	for _, e := range events {
		switch e.typ {
		case "response.output_item.added":
			if item, ok := e.raw["item"].(map[string]interface{}); ok {
				if item["type"] == "function_call" && item["name"] == "get_weather" {
					sawFCAdded = true
				}
			}
		case "response.function_call_arguments.done":
			if e.raw["arguments"] == `{"city":"NYC"}` {
				sawFCArgsDone = true
			}
		case "response.completed":
			sawCompleted = true
		}
	}
	if !sawFCAdded || !sawFCArgsDone || !sawCompleted {
		t.Fatalf("tool stream incomplete: added=%v argsDone=%v completed=%v", sawFCAdded, sawFCArgsDone, sawCompleted)
	}
}

func TestResponsesStreamFailEvent(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	e := newResponsesEmitter(w, "resp_test", "auto", 3)
	e.start()
	e.fail("upstream exploded")
	w.Flush()
	events := parseResponsesSSE(t, buf.String())
	assertMonotonicSeq(t, events)
	last := events[len(events)-1]
	if last.typ != "response.failed" {
		t.Fatalf("want response.failed, got %s", last.typ)
	}
	resp := last.raw["response"].(map[string]interface{})
	if resp["status"] != "failed" {
		t.Errorf("status = %v", resp["status"])
	}
}
