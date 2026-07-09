package main

import (
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
			name: "bare brace tool_calls no fence",
			fragments: []string{
				"Sure.\n",
				"{\"tool_calls\": [{\"name\": \"get_weather\", \"arguments\": {\"city\": \"NYC\"}}]}",
			},
		},
		{
			name: "multiple tool calls in one payload",
			fragments: []string{
				"{\"tool_calls\": [{\"name\": \"a\", \"arguments\": {}}, {\"name\": \"b\", \"arguments\": {\"x\": 1}}]}",
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
