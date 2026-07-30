package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
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
	return "你必须通过工具获取信息或执行操作。你自己无法直接回答，也不能输出 shell 命令、代码片段、搜索步骤等描述性文字——这些都算纯文本，严格禁止。\n\n" +
		"可用工具：\n\n" + mustJSON(tools) + "\n\n" +
		"调用方式：仅输出一个 JSON 代码块，不要任何前缀、说明或解释：\n\n```json\n" +
		`{"tool_calls": [{"name": "工具名", "arguments": {...}}]}` + "\n```\n\n" +
		"无法调用工具时（工具列表为空），直接回复纯文本答案。"
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
// any tool calls.  Three shapes are supported:
//
//  1. A fenced ```json ... ``` block whose body is `{"tool_calls": [...]}`.
//  2. A bare balanced JSON object containing "tool_calls" (model forgot the
//     markdown fences) — found via brace-counting, not a lazy regex.
//  3. GLM-style <tool_call> blocks: tag name is tool name, body is key:value args
//     (one per line, or raw string for the tool to interpret).
//
// Returns type="text" when no valid tool_calls are found.
func parseToolCallOutput(text string) *ParsedToolOutput {
	if strings.TrimSpace(text) == "" {
		return &ParsedToolOutput{Type: "text", PrefixText: text}
	}

	// Path 3 — GLM-style <tool_call> blocks with newline-delimited content.
	if calls, prefix, ok := parseGLMToolCalls(text); ok {
		return &ParsedToolOutput{Type: "tool_calls", PrefixText: prefix, ToolCalls: calls}
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
			after := text[end+1:]
			after = skipDuplicateToolCalls(after)
			return candidate, strings.TrimSpace(text[:start]) + after
		}
	}
	return "", ""
}

// skipDuplicateToolCalls strips a second consecutive tool_calls payload that
// the model emitted immediately after the first one (same JSON repeated with
// no separating prose). The repeated block is silently discarded rather than
// forwarded as plain text.
func skipDuplicateToolCalls(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "{") {
		return s
	}
	depth := 0
	inString := false
	escapeNext := false
	end := -1
	for i := 0; i < len(s); i++ {
		ch := s[i]
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
		return s
	}
	dup := s[:end+1]
	if !strings.Contains(dup, `"tool_calls"`) || !json.Valid([]byte(dup)) {
		return s
	}
	return skipDuplicateToolCalls(s[end+1:])
}

// parseGLMToolCalls detects GLM-style <tool_call> wrappers and converts them
// to the proxy's internal ToolCall structs. The first body line is the tool
// name; the remaining body is its arguments.
func parseGLMToolCalls(text string) (calls []ToolCall, prefix string, ok bool) {
	const openTag = "<tool_call>"
	const closeTag = "</tool_call>"

	searchFrom := 0
	now := time.Now().UnixNano()
	for {
		relStart := strings.Index(strings.ToLower(text[searchFrom:]), openTag)
		if relStart < 0 {
			break
		}
		tagStart := searchFrom + relStart
		bodyStart := tagStart + len(openTag)
		relEnd := strings.Index(strings.ToLower(text[bodyStart:]), closeTag)
		if relEnd < 0 {
			break
		}
		closeIdx := bodyStart + relEnd
		body := strings.TrimSpace(text[bodyStart:closeIdx])
		searchFrom = closeIdx + len(closeTag)

		lineEnd := strings.IndexByte(body, '\n')
		if lineEnd < 0 {
			continue
		}
		toolName := strings.TrimSpace(body[:lineEnd])
		if !validGLMToolName(toolName) {
			continue
		}
		argsBody := strings.TrimSpace(body[lineEnd+1:])
		argsStr := normalizeGLMToolArguments(argsBody)

		if len(calls) == 0 {
			prefix = strings.TrimSpace(text[:tagStart])
		}
		calls = append(calls, ToolCall{
			ID:   fmt.Sprintf("call_%d_%d", now, len(calls)),
			Type: "function",
			Function: ToolCallFunction{
				Name:      toolName,
				Arguments: argsStr,
			},
		})
	}

	if len(calls) == 0 {
		return nil, "", false
	}
	return calls, prefix, true
}

func validGLMToolName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if i == 0 && !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
			return false
		}
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func normalizeGLMToolArguments(body string) string {
	if body == "" {
		return "{}"
	}
	if json.Valid([]byte(body)) {
		var object map[string]interface{}
		if json.Unmarshal([]byte(body), &object) == nil {
			return body
		}
	}

	args := make(map[string]interface{})
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		args[key] = val
	}
	if len(args) == 0 {
		return "{}"
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

var malformedEditCallPattern = regexp.MustCompile(`(\})\s*,\s*("newText"\s*:\s*"(?:\\.|[^"\\])*")\s*,\s*("oldText"\s*:\s*"(?:\\.|[^"\\])*")\s*\}\s*,\s*("path"\s*:)`)

func repairMalformedEditToolCall(payload string) (string, bool) {
	repaired := payload

	// Fix 1: Extract misplaced `"name":"edit"` from inside arguments to the
	// tool-call level. Model emits `},"name":"edit"}]}}` inside the edits
	// array instead of closing the array first and placing name at the
	// tool-call level: `}]},"name":"edit"}]}`.
	if strings.Contains(repaired, `},"name":"edit"}]}}`) {
		repaired = strings.Replace(repaired, `},"name":"edit"}]}}`, `}]},"name":"edit"}]}`, 1)
	}

	// Fix 2: Missing `{` before a second edit object's first field.
	if strings.Contains(repaired, `},"newText":`) {
		repaired = strings.Replace(repaired, `},"newText":`, `},{"newText":`, 1)
	}

	// Fix 3: Close the edits array if `,"path"` is still preceded by a bare `}`.
	if strings.Contains(repaired, `},"path"`) {
		repaired = strings.Replace(repaired, `},"path"`, `]},"path"`, 1)
	}

	var probe struct {
		ToolCalls []struct {
			Name string `json:"name"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal([]byte(repaired), &probe) != nil || len(probe.ToolCalls) != 1 || probe.ToolCalls[0].Name != "edit" {
		return "", false
	}
	return repaired, true
}

// parseToolCallsPayload validates and normalises a `{"tool_calls": [...]}`
// payload into ToolCall structs.
func parseToolCallsPayload(jsonStr string) ([]ToolCall, bool) {
	var wrapper struct {
		ToolCalls []map[string]interface{} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		if repaired, ok := repairMalformedEditToolCall(jsonStr); ok {
			jsonStr = repaired
		} else {
			// Last-ditch: trim to first { … last } and retry.
			first := strings.Index(jsonStr, "{")
			last := strings.LastIndex(jsonStr, "}")
			if first < 0 || last <= first {
				return nil, false
			}
			jsonStr = jsonStr[first : last+1]
		}
		if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
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

// ── Incremental stream classifier ───────────────────────────────────────────
//
// The CLI emits whole JSON objects per stdout line, and each line's extracted
// content is an incremental text fragment. Plain-text fragments must be
// forwarded to the client immediately; a fragment that starts building a
// `{"tool_calls":[...]}` payload must NOT be forwarded as text — it has to be
// held until the payload is complete (or proven not to be one) and turned
// into a proper tool_use/tool_calls event instead.
//
// streamClassifier re-runs the existing whole-string parseToolCallOutput on
// the held suffix after every new fragment rather than hand-rolling an
// incremental brace counter — tool_calls payloads are realistically a few KB
// and fragments arrive at CLI-line granularity, so re-parsing is cheap, and
// this guarantees identical results to the non-streaming code path by
// construction (same function, same priority rules).

type streamClassifier struct {
	held strings.Builder
}

// maxHeldBytes caps how much text we'll hold waiting for a JSON payload to
// balance before giving up and flushing it as plain text (protects against a
// pathological unbalanced `{` never closing for the rest of the response).
const maxHeldBytes = 256 * 1024

// feed appends the next fragment (one CLI stdout line's extracted content
// text). It returns plainText, which the caller must forward immediately as
// a content delta, and resolved, which is non-nil exactly when a complete
// tool_calls payload was just validated. The caller must stop treating
// subsequent feed() output as forwardable content once resolved != nil for
// this classifier instance (there is at most one tool_calls payload per
// response, matching parseToolCallOutput's contract) — it should keep
// draining the underlying scanner to EOF regardless, just without forwarding.
func (s *streamClassifier) feed(fragment string) (plainText string, resolved *ParsedToolOutput) {
	if fragment == "" && s.held.Len() == 0 {
		return "", nil
	}
	candidate := s.held.String() + fragment
	s.held.Reset()

	idx := earliestTriggerIndex(candidate)
	if idx < 0 {
		// No new trigger marker, but the accumulated buffer (held + this
		// fragment) might itself be a complete tool_calls payload — e.g.
		// held contained the full fenced block and fragment is just trailing
		// text. Try parsing before flushing as plain text.
		if parsed := parseToolCallOutput(candidate); parsed.Type == "tool_calls" {
			return parsed.PrefixText, parsed
		}
		return candidate, nil
	}
	safe, tail := candidate[:idx], candidate[idx:]

	// If tail has an opened ```json fence with no closing ``` yet,
	// extractToolCallJSON's Path1 (fenced match) can't succeed OR fail
	// definitively — calling parseToolCallOutput now would let Path2 (bare
	// brace scan) jump ahead, since a balanced `{...}` can appear inside the
	// fence well before the closing marker arrives. That would both resolve
	// too early and leak the fence syntax itself into PrefixText. Hold until
	// the fence closes (or maxHeldBytes gives up) so Path1 gets first look,
	// exactly as it would against the whole buffer.
	if fenceOpenNotClosed(tail) {
		if len(tail) > maxHeldBytes {
			return safe + tail, nil
		}
		s.held.WriteString(tail)
		return safe, nil
	}

	parsed := parseToolCallOutput(tail)
	if parsed.Type == "tool_calls" {
		return safe + parsed.PrefixText, parsed
	}
	if len(tail) > maxHeldBytes {
		return safe + tail, nil
	}
	s.held.WriteString(tail)
	return safe, nil
}

// fenceOpenNotClosed reports whether s contains a ```json fence marker not
// yet followed by a closing ```.
func fenceOpenNotClosed(s string) bool {
	idx := strings.Index(s, "```json")
	if idx < 0 {
		return false
	}
	return !strings.Contains(s[idx+len("```json"):], "```")
}

// flush returns any text still held with no resolution (end of stream
// reached without ever completing/confirming a tool_calls payload) — it is
// plain text that was never provably JSON.
func (s *streamClassifier) flush() string {
	remaining := s.held.String()
	s.held.Reset()
	return remaining
}

// earliestTriggerIndex returns the position of the earliest possible start
// of a tool_calls payload (a fenced ```json marker, a bare '{', a <tool_call>
// tag, or a suffix that could still grow into one of those), or -1 if none
// appears.
func earliestTriggerIndex(s string) int {
	idx := -1
	if fenced := strings.Index(s, "```json"); fenced >= 0 {
		idx = fenced
	}
	if brace := strings.IndexByte(s, '{'); brace >= 0 && (idx < 0 || brace < idx) {
		idx = brace
	}
	if glmt := glmTriggerIndex(s); glmt >= 0 && (idx < 0 || glmt < idx) {
		idx = glmt
	}
	// A fence marker can be split across two fragments (e.g. one fragment
	// ends in "``" and the next starts with "`json"). If the tail of s is a
	// proper prefix of "```json", hold from there instead of forwarding what
	// might turn out to be half a fence marker as plain text.
	if l := partialFenceSuffixLen(s); l > 0 {
		if partialIdx := len(s) - l; idx < 0 || partialIdx < idx {
			idx = partialIdx
		}
	}
	return idx
}

// glmTriggerIndex returns the index of a complete or partial <tool_call>
// marker so split streaming fragments are held until classification is possible.
func glmTriggerIndex(s string) int {
	const marker = "<tool_call>"
	lower := strings.ToLower(s)
	if idx := strings.Index(lower, marker); idx >= 0 {
		return idx
	}
	max := len(marker) - 1
	if len(lower) < max {
		max = len(lower)
	}
	for length := max; length > 0; length-- {
		if strings.HasSuffix(lower, marker[:length]) {
			return len(s) - length
		}
	}
	return -1
}

// partialFenceSuffixLen returns the length of the longest suffix of s that is
// also a non-empty, proper prefix of the "```json" fence marker. Returns 0
// when no such suffix exists.
func partialFenceSuffixLen(s string) int {
	const marker = "```json"
	max := len(marker) - 1
	if len(s) < max {
		max = len(s)
	}
	for l := max; l > 0; l-- {
		if strings.HasSuffix(s, marker[:l]) {
			return l
		}
	}
	return 0
}
