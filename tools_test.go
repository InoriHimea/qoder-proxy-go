package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamClassifierMatchesWholeBufferParse feeds fragments one at a time
// into a streamClassifier and checks the result against calling
// parseToolCallOutput once on the concatenated whole text — the classifier
// must never produce a different tool_calls verdict, or lose/invent text,
// relative to the non-streaming code path.
//
// Text forwarded before a resolved tool_calls block may retain trailing
// whitespace that whole-buffer parsing would have trimmed off the prefix
// (extractToolCallJSON trims prefixText) — true streaming can't know in
// advance that a trailing newline is about to be followed by a JSON block
// rather than more prose, so it forwards eagerly and never retroactively
// trims already-sent text. Comparisons account for that with TrimSpace.
func TestStreamClassifierMatchesWholeBufferParse(t *testing.T) {
	tests := []struct {
		name      string
		fragments []string
	}{
		{
			name:      "plain text single fragment",
			fragments: []string{"hello world, nothing special here."},
		},
		{
			name:      "plain text many fragments",
			fragments: []string{"hello ", "world, ", "nothing ", "special ", "here."},
		},
		{
			name: "fenced tool_calls single fragment",
			fragments: []string{
				"Sure, calling a tool.\n```json\n{\"tool_calls\": [{\"name\": \"get_weather\", \"arguments\": {\"city\": \"NYC\"}}]}\n```\n",
			},
		},
		{
			name: "fenced tool_calls split across many fragments",
			fragments: []string{
				"Sure, calling a tool.\n",
				"```json\n",
				"{\"tool_calls\": ",
				"[{\"name\": \"get_weather\", ",
				"\"arguments\": {\"city\": \"NYC\"}}]}\n",
				"```\n",
			},
		},
		{
			name: "fence marker split mid-token",
			fragments: []string{
				"Sure, calling a tool.\n``",
				"`json\n{\"tool_calls\": [{\"name\": \"get_weather\", \"arguments\": {\"city\": \"NYC\"}}]}\n```\n",
			},
		},
		{
			name: "multiple tool calls in one payload",
			fragments: []string{
				"{\"tool_calls\": [{\"name\": \"a\", \"arguments\": {}}, {\"name\": \"b\", \"arguments\": {\"x\": 1}}]}",
			},
		},
		{
			name: "duplicate tool_calls (model emitted same JSON twice in one block)",
			fragments: []string{
				"{\"tool_calls\": [{\"name\": \"view\", \"arguments\": {\"file_path\": \"go.mod\"}}]}{\"tool_calls\": [{\"name\": \"view\", \"arguments\": {\"file_path\": \"go.mod\"}}]}",
			},
		},
		{
			name: "unbalanced brace never closes, falls through at EOF",
			fragments: []string{
				"here is a {code snippet that never closes and just keeps going as plain prose without any real json structure at all",
			},
		},
		{
			name: "unbalanced brace across many fragments",
			fragments: []string{
				"here is a {code ",
				"snippet that never ",
				"closes and just keeps ",
				"going as plain prose",
			},
		},
		{
			name: "prose brace before real fenced tool_calls",
			fragments: []string{
				"this is a {sample} of text before the real payload.\n",
				"```json\n{\"tool_calls\": [{\"name\": \"noop\", \"arguments\": {}}]}\n```\n",
			},
		},
		{
			name:      "empty fragment is a no-op",
			fragments: []string{"hello", "", " world"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			full := strings.Join(tt.fragments, "")
			want := parseToolCallOutput(full)

			c := &streamClassifier{}
			var forwarded strings.Builder
			var resolved *ParsedToolOutput
			for _, frag := range tt.fragments {
				if resolved != nil {
					t.Fatalf("feed called after resolution")
				}
				plain, r := c.feed(frag)
				forwarded.WriteString(plain)
				if r != nil {
					resolved = r
				}
			}
			if resolved == nil {
				forwarded.WriteString(c.flush())
			}

			if want.Type == "tool_calls" {
				if resolved == nil {
					t.Fatalf("expected tool_calls resolution, got none; forwarded=%q", forwarded.String())
				}
				if got := strings.TrimSpace(forwarded.String()); got != want.PrefixText {
					t.Errorf("forwarded text (trimmed) = %q, want prefix %q", got, want.PrefixText)
				}
				if len(resolved.ToolCalls) != len(want.ToolCalls) {
					t.Fatalf("tool call count = %d, want %d", len(resolved.ToolCalls), len(want.ToolCalls))
				}
				for i := range want.ToolCalls {
					if resolved.ToolCalls[i].Function.Name != want.ToolCalls[i].Function.Name {
						t.Errorf("tool[%d] name = %q, want %q", i, resolved.ToolCalls[i].Function.Name, want.ToolCalls[i].Function.Name)
					}
					if resolved.ToolCalls[i].Function.Arguments != want.ToolCalls[i].Function.Arguments {
						t.Errorf("tool[%d] arguments = %q, want %q", i, resolved.ToolCalls[i].Function.Arguments, want.ToolCalls[i].Function.Arguments)
					}
				}
			} else {
				if resolved != nil {
					t.Fatalf("unexpected tool_calls resolution: %+v", resolved)
				}
				if forwarded.String() != full {
					t.Errorf("forwarded text = %q, want full text %q", forwarded.String(), full)
				}
			}
		})
	}
}

func TestStreamClassifierMaxHeldBytesFallback(t *testing.T) {
	c := &streamClassifier{}
	huge := "{" + strings.Repeat("a", maxHeldBytes+10)
	plain, resolved := c.feed(huge)
	if resolved != nil {
		t.Fatalf("did not expect resolution for oversized unbalanced brace")
	}
	if plain != huge {
		t.Errorf("expected the oversized held text to be flushed as plain text immediately, got len=%d want len=%d", len(plain), len(huge))
	}
}

func TestStreamClassifierMaxHeldBytesFallbackAcrossFragments(t *testing.T) {
	c := &streamClassifier{}
	var forwarded strings.Builder
	plain, resolved := c.feed("{start of an unbalanced object ")
	forwarded.WriteString(plain)
	if resolved != nil {
		t.Fatalf("did not expect early resolution")
	}
	chunk := strings.Repeat("a", 4096)
	for i := 0; i < maxHeldBytes/len(chunk)+2; i++ {
		plain, resolved = c.feed(chunk)
		forwarded.WriteString(plain)
		if resolved != nil {
			t.Fatalf("did not expect resolution for unbalanced object")
		}
	}
	forwarded.WriteString(c.flush())
	if forwarded.Len() == 0 {
		t.Fatalf("expected forwarded text to be non-empty")
	}
}

func TestParseGLMToolCallWrapper(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantName   string
		wantArgs   string
		wantPrefix string
	}{
		{
			name:       "bash key value arguments",
			output:     "I'll inspect it.\n<tool_call>bash\ncommand: ls\n</tool_call>",
			wantName:   "bash",
			wantArgs:   `{"command":"ls"}`,
			wantPrefix: "I'll inspect it.",
		},
		{
			name:     "Write JSON arguments",
			output:   "<tool_call>Write\n{\"file_path\":\"note.txt\",\"content\":\"hello\"}\n</tool_call>",
			wantName: "Write",
			wantArgs: `{"file_path":"note.txt","content":"hello"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := parseToolCallOutput(tt.output)
			if parsed.Type != "tool_calls" || len(parsed.ToolCalls) != 1 {
				t.Fatalf("parsed = %+v, want one tool call", parsed)
			}
			call := parsed.ToolCalls[0]
			if call.Function.Name != tt.wantName {
				t.Errorf("tool name = %q, want %q", call.Function.Name, tt.wantName)
			}
			if call.Function.Arguments != tt.wantArgs {
				t.Errorf("arguments = %q, want %q", call.Function.Arguments, tt.wantArgs)
			}
			if parsed.PrefixText != tt.wantPrefix {
				t.Errorf("prefix = %q, want %q", parsed.PrefixText, tt.wantPrefix)
			}
		})
	}
}

func TestStreamClassifierGLMToolCallWrapper(t *testing.T) {
	c := &streamClassifier{}
	fragments := []string{"I'll inspect it.\n<tool_", "call>Write\n{\"file_path\":", "\"note.txt\",\"content\":\"hello\"}\n</tool_call>"}
	var forwarded strings.Builder
	var resolved *ParsedToolOutput
	for _, fragment := range fragments {
		plain, result := c.feed(fragment)
		forwarded.WriteString(plain)
		if result != nil {
			resolved = result
		}
	}
	if resolved == nil || len(resolved.ToolCalls) != 1 {
		t.Fatalf("resolved = %+v, want one tool call", resolved)
	}
	if resolved.ToolCalls[0].Function.Name != "Write" {
		t.Errorf("tool name = %q, want Write", resolved.ToolCalls[0].Function.Name)
	}
	if got := strings.TrimSpace(forwarded.String()); got != "I'll inspect it." {
		t.Errorf("forwarded = %q", got)
	}
}

func TestParseMalformedEditToolCall(t *testing.T) {
	// Model malformed output: `"name":"edit"` is inside the edits array
	// instead of at the tool-call level, and the array is missing `]` before it.
	// Also the edits object is missing `}` at the end (extra `}` instead of `]`).
	// Correct JSON: {"edits":[...],"name":"edit"}
	// Malformed:    {"edits":[...},"name":"edit"}]}}
	content := "```json\n" +
		`{"tool_calls":[{"arguments":{"edits":[{"newText":"new 1","oldText":"old 1","path":"F:/a.json"},"name":"edit"}]}}` +
		"\n```"

	parsed := parseToolCallOutput(content)
	if parsed.Type != "tool_calls" || len(parsed.ToolCalls) != 1 {
		t.Fatalf("parsed = %+v, want one tool call", parsed)
	}
	call := parsed.ToolCalls[0]
	if call.Function.Name != "edit" {
		t.Fatalf("tool name = %q, want edit", call.Function.Name)
	}
	wantArgs := `{"edits":[{"newText":"new 1","oldText":"old 1","path":"F:/a.json"}]}`
	if call.Function.Arguments != wantArgs {
		t.Fatalf("arguments = %q, want %q", call.Function.Arguments, wantArgs)
	}
}

func TestStreamClassifierRepairsMalformedEditToolCall(t *testing.T) {
	fragments := []string{
		"```json\n",
		`{"tool_calls":[{"arguments":{"edits":[{"newText":"new1","oldText":"old1","path":"F:/a.json"},"`,
		`name":"edit"}]}}`,
		"\n```",
	}
	classifier := &streamClassifier{}
	var forwarded strings.Builder
	var resolved *ParsedToolOutput
	for _, fragment := range fragments {
		plain, result := classifier.feed(fragment)
		forwarded.WriteString(plain)
		if result != nil {
			resolved = result
		}
	}
	if resolved == nil || len(resolved.ToolCalls) != 1 {
		t.Fatalf("resolved = %+v, want one tool call; forwarded=%q", resolved, forwarded.String())
	}
	if resolved.ToolCalls[0].Function.Name != "edit" {
		t.Fatalf("tool name = %q, want edit", resolved.ToolCalls[0].Function.Name)
	}
	response := buildAnthropicResponse("msg_test", "glm-5.2", strings.Join(fragments, ""), resolved, 10, "")
	if response.StopReason != "tool_use" || len(response.Content) != 1 || response.Content[0].Type != "tool_use" {
		t.Fatalf("response = %+v", response)
	}
}

func TestParseMalformedNonEditToolCallStaysText(t *testing.T) {
	content := `{"tool_calls":[{"arguments":{"items":[{"value":"new"},"oldValue":"old"}},"name":"other"}]}`
	parsed := parseToolCallOutput(content)
	if parsed.Type != "text" {
		t.Fatalf("parsed = %+v, want text", parsed)
	}
}

func TestBuildDirectChatMessageKeepsArgumentsString(t *testing.T) {
	message := buildDirectChatMessage("<tool_call>Write\n{\"file_path\":\"note.txt\",\"content\":\"hello\"}\n</tool_call>", "")
	calls, ok := message["tool_calls"].([]map[string]interface{})
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", message["tool_calls"])
	}
	function, ok := calls[0]["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("function = %#v", calls[0]["function"])
	}
	arguments, ok := function["arguments"].(string)
	if !ok {
		t.Fatalf("arguments type = %T, want string", function["arguments"])
	}
	if arguments != `{"file_path":"note.txt","content":"hello"}` {
		t.Fatalf("arguments = %q", arguments)
	}
}

func TestBuildAnthropicResponseFromGLMWriteCall(t *testing.T) {
	content := "<tool_call>Write\n{\"file_path\":\"note.txt\",\"content\":\"hello\"}\n</tool_call>"
	response := buildAnthropicResponse("msg_test", "glm-5.2", content, parseToolCallOutput(content), 10, "")
	if response.StopReason != "tool_use" || len(response.Content) != 1 {
		t.Fatalf("response = %+v", response)
	}
	block := response.Content[0]
	if block.Type != "tool_use" || block.Name != "Write" {
		t.Fatalf("tool block = %+v", block)
	}
	input, ok := block.Input.(map[string]interface{})
	if !ok {
		t.Fatalf("input type = %T", block.Input)
	}
	if input["file_path"] != "note.txt" || input["content"] != "hello" {
		t.Fatalf("input = %#v", input)
	}
}

func TestNormalizeAnthropicToolUseMatchesExpectedOutputFormat(t *testing.T) {
	content := []interface{}{
		map[string]interface{}{
			"type":  "tool_use",
			"id":    "toolu_test",
			"name":  "Write",
			"input": map[string]interface{}{"file_path": "round2.txt", "content": "turn two"},
		},
	}

	got := normalizeAnthropicContent(content)
	if strings.Contains(got, "<tool_use") {
		t.Fatalf("history must not contain XML tool_use tags: %q", got)
	}
	if !strings.HasPrefix(got, "```json\n") || !strings.HasSuffix(got, "\n```") {
		t.Fatalf("history must use fenced JSON: %q", got)
	}
	parsed := parseToolCallOutput(got)
	if parsed.Type != "tool_calls" || len(parsed.ToolCalls) != 1 {
		t.Fatalf("history must parse as one tool call: %+v", parsed)
	}
	call := parsed.ToolCalls[0]
	if call.Function.Name != "Write" {
		t.Fatalf("tool name = %q, want Write", call.Function.Name)
	}
	if call.Function.Arguments != `{"content":"turn two","file_path":"round2.txt"}` {
		t.Fatalf("arguments = %q", call.Function.Arguments)
	}
}

// TestParseToolCallsPayloadRepairsInvalidEscapes verifies that parseToolCallsPayload
// can recover from JSON with invalid escape sequences (e.g. \. \() that the model
// emitted without proper double-escaping. These are common when regex patterns
// appear in tool arguments.
func TestParseToolCallsPayloadRepairsInvalidEscapes(t *testing.T) {
	// The leaked JSON from the bug report: name inside arguments, plus invalid escapes
	invalid := `{"tool_calls":[{"arguments":{"path":"F:/VsCodeProject/agent-platform/src","pattern":"\\.min\(60\)|\\.min\(8\)|\\.min\(40\)|\\.min\(80\)","name":"grep"}}]}`

	calls, ok := parseToolCallsPayload(invalid)
	if !ok {
		t.Fatalf("parseToolCallsPayload(invalid) = false, want true")
	}
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}

	c := calls[0]
	if c.Function.Name != "grep" {
		t.Errorf("tool name = %q, want grep", c.Function.Name)
	}

	// Verify pattern was repaired and preserved correctly
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
		t.Fatalf("unmarshal arguments = %v", err)
	}
	pattern, ok := args["pattern"].(string)
	if !ok {
		t.Fatal("pattern not found in arguments")
	}
	// After repair, \\.(backslash-dot) becomes a literal backslash-dot in the string value
	wantPattern := `\.min\(60\)|\.min\(8\)|\.min\(40\)|\.min\(80\)`
	if pattern != wantPattern {
		t.Errorf("pattern = %q, want %q", pattern, wantPattern)
	}

	// Verify name was extracted from arguments and removed
	_, hasName := args["name"]
	if hasName {
		t.Error("name should have been extracted from arguments and removed")
	}

	// Also test valid JSON with invalid escapes (no name-in-arguments issue)
	validJSONWithBadEscapes := `{"tool_calls":[{"name":"test","arguments":{"regex":"\\.foo\(bar\)"}}]}`
	calls2, ok2 := parseToolCallsPayload(validJSONWithBadEscapes)
	if !ok2 || len(calls2) != 1 {
		t.Fatalf("parseToolCallsPayload(validJSONWithBadEscapes) = %v, %d, want true, 1", ok2, len(calls2))
	}
	if calls2[0].Function.Name != "test" {
		t.Errorf("name = %q, want test", calls2[0].Function.Name)
	}
}

// TestParseToolCallsPayloadNameInArgumentsFallback verifies that when models place
// "name" inside "arguments" instead of at the tool-call level, we extract it.
func TestParseToolCallsPayloadNameInArgumentsFallback(t *testing.T) {
	// Correct schema: name at tool-call level
	correct := `{"tool_calls":[{"name":"search","arguments":{"query":"hello"}}]}`
	calls, ok := parseToolCallsPayload(correct)
	if !ok || len(calls) != 1 || calls[0].Function.Name != "search" {
		t.Fatalf("correct schema failed: %v, %d", ok, len(calls))
	}

	// Wrong schema: name inside arguments
	wrong := `{"tool_calls":[{"arguments":{"query":"world","name":"search2"}}]}`
	calls, ok = parseToolCallsPayload(wrong)
	if !ok || len(calls) != 1 {
		t.Fatalf("wrong schema = %v, %d, want true, 1", ok, len(calls))
	}
	if calls[0].Function.Name != "search2" {
		t.Errorf("extracted name = %q, want search2", calls[0].Function.Name)
	}

	// Verify name was removed from arguments
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("unmarshal = %v", err)
	}
	if _, has := args["name"]; has {
		t.Error("name should be removed from arguments after extraction")
	}
	if query, ok := args["query"].(string); !ok || query != "world" {
		t.Errorf("query = %v, want world", args["query"])
	}

	// Both fields in wrong position should still work
	bothWrong := `{"tool_calls":[{"arguments":{"name":"multi","other":"value"}}]}`
	calls, ok = parseToolCallsPayload(bothWrong)
	if !ok || len(calls) != 1 || calls[0].Function.Name != "multi" {
		t.Fatalf("both wrong = %v, %d, want true, 1", ok, len(calls))
	}
}


// TestParseToolCallOutputEndToEndLeakedJSON verifies the full pipeline:
// extractToolCallJSON (with escape repair) → parseToolCallsPayload (with
// name-in-arguments fallback) resolves tool_calls that would otherwise leak
// as plain text.
func TestParseToolCallOutputEndToEndLeakedJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantName string
	}{
		{
			name:     "regex_pattern_with_invalid_escapes_and_name_in_args",
			input:    `{"tool_calls":[{"arguments":{"path":"F:/VsCodeProject/agent-platform/src","pattern":"\.min\(60\)|\.min\(8\)|\.min\(40\)|\.min\(80\)","name":"grep"}}]}`,
			wantName: "grep",
		},
		{
			name:     "second_leaked_json_with_brackets",
			input:    `{"tool_calls":[{"arguments":{"path":"F:/VsCodeProject/agent-platform/src","pattern":"\.\[.*\.\min\(","name":"grep"}}]}`,
			wantName: "grep",
		},
		{
			name:     "literal_newline_inside_command_string",
			input:    "{\"tool_calls\":[{\"arguments\":{\"command\":\"cd /f/VsCodeProject/dev-linux-builder && uv run pytest -q --tb=no 2>&1 | tail\n-20\",\"i\":\"run tests\",\"timeout\":120},\"name\":\"bash\"}]}",
			wantName: "bash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := parseToolCallOutput(tt.input)
			if parsed.Type != "tool_calls" {
				t.Fatalf("Type = %q, want tool_calls (JSON would leak as text)", parsed.Type)
			}
			if len(parsed.ToolCalls) != 1 {
				t.Fatalf("len(ToolCalls) = %d, want 1", len(parsed.ToolCalls))
			}
			tc := parsed.ToolCalls[0]
			if tc.Function.Name != tt.wantName {
				t.Errorf("name = %q, want %q", tc.Function.Name, tt.wantName)
			}
			// Verify arguments don't contain "name" (it was extracted)
			var args map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				t.Fatalf("unmarshal arguments: %v", err)
			}
			if _, has := args["name"]; has {
				t.Error("name should be removed from arguments after extraction")
			}
		})
	}
}

