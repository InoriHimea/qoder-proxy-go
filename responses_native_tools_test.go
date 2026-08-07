package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The CN gateway can answer with native streaming `delta.tool_calls` instead
// of a text-encoded JSON block. Arguments arrive one fragment at a time and
// only the first fragment carries id + function.name. Later fragments may
// carry only a single `arguments` string with an empty function. A separate
// choice with `finish_reason:"tool_calls"` and no tool_calls at all signals
// that all fragments have been sent.
func nativeToolCallChunks() []ChatChunk {
	raw := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_native_1","type":"function","function":{"name":"list","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"index":0,"id":"","type":"function","function":{"arguments":"{\"path"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"","tool_calls":[{"index":0,"id":"","type":"function","function":{"arguments":"\": \".\"}"}}]}}]}`,
		// finish_reason arrives on a choice with NO tool_calls array.
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	out := make([]ChatChunk, 0, len(raw))
	for _, r := range raw {
		var c ChatChunk
		if err := json.Unmarshal([]byte(r), &c); err != nil {
			panic(err)
		}
		out = append(out, c)
	}
	return out
}

func TestNativeToolCallAccumulatorJoinsFragments(t *testing.T) {
	acc := newNativeToolCallAccumulator()
	for _, chunk := range nativeToolCallChunks() {
		for _, choice := range chunk.Choices {
			acc.feed(choice)
			if acc.resolved() {
				break
			}
		}
		if acc.resolved() {
			break
		}
	}

	resolved := acc.result()
	if resolved == nil || len(resolved.ToolCalls) != 1 {
		t.Fatalf("resolved = %+v, want one tool call", resolved)
	}
	tc := resolved.ToolCalls[0]
	if tc.Function.Name != "list" {
		t.Fatalf("name = %q, want list", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"path": "."}` {
		t.Fatalf("arguments = %q", tc.Function.Arguments)
	}
	if tc.ID != "call_native_1" {
		t.Fatalf("id = %q, want call_native_1", tc.ID)
	}
}

func TestNativeToolCallAccumulatorIgnoresPlainText(t *testing.T) {
	acc := newNativeToolCallAccumulator()
	acc.feed(ChatChunkChoice{Delta: ChatChunkDelta{Content: "hello"}})
	if acc.resolved() {
		t.Fatal("plain text resolved a tool call")
	}
}

// A missing id must still yield a usable call — clients key follow-up tool
// results off call_id, so an empty one breaks the round trip.
func TestNativeToolCallAccumulatorSynthesizesMissingID(t *testing.T) {
	acc := newNativeToolCallAccumulator()
	acc.feed(ChatChunkChoice{Delta: ChatChunkDelta{ToolCalls: []interface{}{
		map[string]interface{}{
			"index":    float64(0),
			"function": map[string]interface{}{"name": "glob", "arguments": `{}`},
		},
	}}})
	// finish_reason arrives on a separate choice with no tool_calls at all.
	acc.feed(ChatChunkChoice{FinishReason: strPtr("tool_calls")})

	if !acc.resolved() {
		t.Fatal("accumulator did not resolve")
	}
	resolved := acc.result()
	if resolved == nil || len(resolved.ToolCalls) != 1 {
		t.Fatalf("resolved = %+v, want one tool call", resolved)
	}
	if resolved.ToolCalls[0].ID == "" {
		t.Fatal("call id is empty")
	}
}

// End-to-end: the Responses emitter must turn native tool_calls into
// function_call output items, not drop them and report a text-only answer.
func TestResponsesEmitterEmitsNativeToolCalls(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	e := newResponsesEmitter(w, "resp_native", "glm-5.2", 5)
	e.start()

	acc := newNativeToolCallAccumulator()
	for _, chunk := range nativeToolCallChunks() {
		for _, choice := range chunk.Choices {
			acc.feed(choice)
			if acc.resolved() {
				break
			}
		}
		if acc.resolved() {
			break
		}
	}
	resolved := acc.result()
	if resolved == nil {
		t.Fatal("accumulator produced no tool calls")
	}
	e.emitResolvedToolCalls(resolved)
	e.finish(&streamClassifier{}, resolved)
	w.Flush()

	out := buf.String()
	for _, want := range []string{
		`"type":"function_call"`,
		`"name":"list"`,
		`response.function_call_arguments.done`,
		`response.completed`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"status":"failed"`) {
		t.Fatalf("stream reported failure:\n%s", out)
	}
}
