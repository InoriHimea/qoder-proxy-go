package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

// ── OpenAI Responses protocol ───────────────────────────────────────────────
//
// codex CLI 0.144.x forces wire_api="responses": it POSTs {input,instructions,
// tools,...} and expects a `response` object (non-stream) or named SSE events
// with a monotonic `sequence_number` and NO `data: [DONE]` sentinel (stream).
// The old alias to handleChatCompletions returned a `chat.completion` shape +
// `[DONE]`, which the responses parser rejects.
//
// This file mirrors the Anthropic-over-CN implementation (anthropic.go +
// direct.go handleAnthropicChatCN/handleAnthropicStreamCN): translate the
// protocol request into the internal ChatRequest, run it through the exact
// same CLI / CN-direct pipeline, then re-serialize the aggregated output into
// Responses shapes. Tool calls ride the existing prompt-injection protocol
// (buildToolSystemPromptFromRaw + parseToolCallOutput), encoded in history as
// fenced JSON so parseToolCallOutput can read them back — never the gateway's
// native tool-calling fields.

// ResponsesRequest is the subset of the OpenAI Responses request body the
// proxy consumes. Unknown fields (store, metadata, temperature, …) are
// tolerated and mostly ignored — this is a stateless translator.
type ResponsesRequest struct {
	Model        string      `json:"model"`
	Input        interface{} `json:"input"` // string OR []item
	Instructions string      `json:"instructions,omitempty"`
	Stream       bool        `json:"stream,omitempty"`
	// Responses names the output cap max_output_tokens; accept max_tokens too.
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	MaxTokens       int                 `json:"max_tokens,omitempty"`
	Tools           json.RawMessage     `json:"tools,omitempty"`
	ToolChoice      interface{}         `json:"tool_choice,omitempty"`
	Reasoning       *ResponsesReasoning `json:"reasoning,omitempty"`
	// Stateful fields the proxy cannot honor — presence is logged, then ignored.
	Store              *bool  `json:"store,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

func (r ResponsesRequest) maxTokens() int {
	if r.MaxOutputTokens > 0 {
		return r.MaxOutputTokens
	}
	return r.MaxTokens
}

func (r ResponsesRequest) reasoningEffort() string {
	if r.Reasoning != nil {
		return r.Reasoning.Effort
	}
	return ""
}

// ── Response / output-item shapes ───────────────────────────────────────────

type ResponsesResponse struct {
	ID        string                `json:"id"`
	Object    string                `json:"object"` // "response"
	CreatedAt int64                 `json:"created_at"`
	Status    string                `json:"status"` // "in_progress" | "completed" | "failed"
	Model     string                `json:"model"`
	Output    []ResponsesOutputItem `json:"output"`
	Usage     *ResponsesUsage       `json:"usage,omitempty"`
	Error     *ResponsesError       `json:"error,omitempty"`
}

type ResponsesError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ResponsesOutputItem struct {
	Type   string `json:"type"` // "message" | "function_call" | "reasoning"
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`

	// message
	Role    string                 `json:"role,omitempty"`
	Content []ResponsesContentPart `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// reasoning
	Summary []ResponsesSummaryPart `json:"summary,omitempty"`
}

type ResponsesContentPart struct {
	Type        string        `json:"type"` // "output_text"
	Text        string        `json:"text"`
	Annotations []interface{} `json:"annotations"`
}

type ResponsesSummaryPart struct {
	Type string `json:"type"` // "summary_text"
	Text string `json:"text"`
}

type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ── Request → ChatRequest ───────────────────────────────────────────────────

// flattenResponsesContent flattens a Responses content field (a plain string
// or an array of typed parts: input_text/output_text/text/input_image/…) into
// a single text blob the CLI can read.
func flattenResponsesContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	arr, ok := content.([]interface{})
	if !ok {
		return fmt.Sprintf("%v", content)
	}
	var parts []string
	for _, raw := range arr {
		if s, ok := raw.(string); ok {
			if s != "" {
				parts = append(parts, s)
			}
			continue
		}
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch t, _ := m["type"].(string); t {
		case "input_text", "output_text", "text", "":
			if s, ok := m["text"].(string); ok && s != "" {
				parts = append(parts, s)
			}
		case "input_image":
			parts = append(parts, "[image]")
		case "input_file":
			label, _ := m["filename"].(string)
			if label == "" {
				label = "file"
			}
			parts = append(parts, fmt.Sprintf("[file: %s]", label))
		case "refusal":
			if s, ok := m["refusal"].(string); ok && s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// parseResponsesInput flattens the Responses `input` field into []Message.
// `input` is either a plain string (a single user turn) or an array of items:
//
//   - {type:"message", role, content}          → a role message
//   - {type:"function_call", call_id, name, arguments}
//     → assistant turn re-encoded as the fenced-JSON tool-call protocol so
//     parseToolCallOutput can read it back (matches normalizeAnthropicContent)
//   - {type:"function_call_output", call_id, output}
//     → a <tool_result id="…">…</tool_result> block
//   - {type:"reasoning", …}                    → dropped (not fed back to CLI)
func parseResponsesInput(input interface{}) []Message {
	if input == nil {
		return nil
	}
	if s, ok := input.(string); ok {
		if s == "" {
			return nil
		}
		return []Message{{Role: "user", Content: s}}
	}
	arr, ok := input.([]interface{})
	if !ok {
		return nil
	}

	var messages []Message
	for _, raw := range arr {
		m, ok := raw.(map[string]interface{})
		if !ok {
			if s, ok := raw.(string); ok && s != "" {
				messages = append(messages, Message{Role: "user", Content: s})
			}
			continue
		}
		switch t, _ := m["type"].(string); t {
		case "message", "":
			role, _ := m["role"].(string)
			if role == "" {
				role = "user"
			}
			if strings.ToLower(role) == "developer" {
				role = "system"
			}
			text := flattenResponsesContent(m["content"])
			if text != "" {
				messages = append(messages, Message{Role: role, Content: text})
			}
		case "function_call":
			name, _ := m["name"].(string)
			argsStr, _ := m["arguments"].(string)
			var argsVal interface{}
			if argsStr != "" && json.Unmarshal([]byte(argsStr), &argsVal) != nil {
				argsVal = map[string]interface{}{}
			}
			if argsVal == nil {
				argsVal = map[string]interface{}{}
			}
			callJSON, err := json.Marshal(map[string]interface{}{
				"tool_calls": []map[string]interface{}{
					{"name": name, "arguments": argsVal},
				},
			})
			if err != nil {
				callJSON = []byte(fmt.Sprintf(`{"tool_calls":[{"name":%q,"arguments":{}}]}`, name))
			}
			messages = append(messages, Message{
				Role:    "assistant",
				Content: fmt.Sprintf("```json\n%s\n```", string(callJSON)),
			})
		case "function_call_output":
			callID, _ := m["call_id"].(string)
			output := flattenResponsesContent(m["output"])
			if output == "" {
				if s, ok := m["output"].(string); ok {
					output = s
				}
			}
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: callID,
				Content:    output,
			})
		case "reasoning":
			// Prior reasoning is not replayed to the CLI.
		}
	}
	return messages
}

// responsesRequestToChatRequest converts a Responses request into the internal
// ChatRequest shape so it flows through the existing CLI / CN pipeline. The
// top-level `instructions` field is merged into the system message (ahead of
// the tool-protocol prompt).
func responsesRequestToChatRequest(req ResponsesRequest, toolPrompt string) ChatRequest {
	fullSystem := mergeSystemPrompt(strings.TrimSpace(req.Instructions), toolPrompt)

	var messages []Message
	if fullSystem != "" {
		messages = append(messages, Message{Role: "system", Content: fullSystem})
	}
	messages = append(messages, parseResponsesInput(req.Input)...)

	return ChatRequest{
		Model:           req.Model,
		Messages:        messages,
		Stream:          req.Stream,
		MaxTokens:       req.maxTokens(),
		ReasoningEffort: req.reasoningEffort(),
		Tools:           normalizeOpenAITools(req.Tools),
		ToolChoice:      req.ToolChoice,
	}
}

// ── Non-streaming response builder ──────────────────────────────────────────

// buildResponsesOutput assembles the ordered output-item list: an optional
// leading reasoning item, then either a message item (plain text) or
// function_call items (tool calls, preceded by any prefix text as a message).
func buildResponsesOutput(content string, parsed *ParsedToolOutput, thinking string) []ResponsesOutputItem {
	var out []ResponsesOutputItem
	if thinking != "" {
		out = append(out, ResponsesOutputItem{
			Type:    "reasoning",
			ID:      generateCallId("rs_"),
			Summary: []ResponsesSummaryPart{{Type: "summary_text", Text: thinking}},
		})
	}
	if parsed != nil && parsed.Type == "tool_calls" {
		if parsed.PrefixText != "" {
			out = append(out, responsesMessageItem(parsed.PrefixText))
		}
		for _, tc := range parsed.ToolCalls {
			out = append(out, ResponsesOutputItem{
				Type:      "function_call",
				ID:        generateCallId("fc_"),
				Status:    "completed",
				CallID:    tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		return out
	}
	out = append(out, responsesMessageItem(content))
	return out
}

func responsesMessageItem(text string) ResponsesOutputItem {
	return ResponsesOutputItem{
		Type:   "message",
		ID:     generateCallId("msg_"),
		Status: "completed",
		Role:   "assistant",
		Content: []ResponsesContentPart{
			{Type: "output_text", Text: text, Annotations: []interface{}{}},
		},
	}
}

func buildResponsesResponse(id, model, content string, parsed *ParsedToolOutput, inputTokens int, thinking string) ResponsesResponse {
	outputTokens := countTokens(thinking + content)
	return ResponsesResponse{
		ID:        id,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     model,
		Output:    buildResponsesOutput(content, parsed, thinking),
		Usage: &ResponsesUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			TotalTokens:  inputTokens + outputTokens,
		},
	}
}

// ── Entry point ─────────────────────────────────────────────────────────────

func handleResponses(ctx *fasthttp.RequestCtx, cm *ConfigManager, um *UsageManager, dc *DirectClient) {
	started := time.Now()
	var req ResponsesRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		ctx.Error("Invalid JSON", http.StatusBadRequest)
		um.Record("unknown", 0, 0, true, time.Since(started).Milliseconds())
		return
	}

	if req.Store != nil && *req.Store || req.PreviousResponseID != "" {
		AddSystemLog("Responses request used store/previous_response_id; proxy is stateless, these are ignored", "warn", "responses")
	}

	cfg := cm.Get()
	if cfg.UseDirectAPI {
		if strings.ToLower(cfg.Backend) == "cn" {
			AddSystemLog(fmt.Sprintf("Using Direct API (CN) for Responses protocol, model %s", req.Model), "info", "direct")
			if !dc.HandleResponsesChat(ctx, req, um) {
				return
			}
			AddSystemLog("Direct API (CN) failed, falling back to CLI mode automatically...", "info", "system")
		} else {
			AddSystemLog("Direct API requested but not implemented for Responses protocol on this backend. Falling back to CLI.", "warn", "direct")
		}
	}

	var toolPrompt string
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, false)
	}
	chatReq := responsesRequestToChatRequest(req, toolPrompt)
	sys, prompt := extractSystemAndPrompt(chatReq.Messages)
	inputTokens := countTokens(sys + "\n" + prompt)

	// sys already carries instructions+toolPrompt (folded in by
	// responsesRequestToChatRequest), so it IS the full system text — do not
	// re-merge toolPrompt or it would be applied twice.
	opts := SpawnOptions{
		Model:           req.Model,
		ReasoningEffort: req.reasoningEffort(),
		MaxTokens:       req.maxTokens(),
		SystemPrompt:    sys,
		DisableTools:    true,
	}

	stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
	if err != nil {
		ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
		um.Record(req.Model, inputTokens, 0, true, time.Since(started).Milliseconds())
		return
	}

	id := fmt.Sprintf("resp_%d", time.Now().UnixNano())

	if req.Stream {
		ctx.SetUserValue("response_body", "[Streaming Response...]")
		handleResponsesStreamCLI(ctx, req, stdout, id, inputTokens, um, started)
		return
	}

	defer stdout.Close()
	content, thinking, cliErr := drainCLIContent(stdout)
	if cliErr != "" {
		ctx.Error(fmt.Sprintf("CLI Error: %s", cliErr), http.StatusUnauthorized)
		um.Record(req.Model, inputTokens, 0, true, time.Since(started).Milliseconds())
		return
	}

	parsed := parseToolCallOutput(content)
	respData := buildResponsesResponse(id, req.Model, content, parsed, inputTokens, thinking)

	ctx.SetContentType("application/json")
	ctx.SetUserValue("response_body", respData)
	json.NewEncoder(ctx).Encode(respData)

	outputTokens := countTokens(thinking + content)
	um.Record(req.Model, inputTokens, outputTokens, false, time.Since(started).Milliseconds())
	ctx.SetUserValue("log_metrics", &LogMetrics{
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		ThinkingTokens: countTokens(thinking),
	})
}

// drainCLIContent reads a qodercli stdout stream (whole-JSON-object lines) to
// EOF, aggregating assistant content and thinking text. Returns the CLI error
// result string when the CLI emitted an is_error line.
func drainCLIContent(stdout interface{ Read([]byte) (int, error) }) (content, thinking, cliErr string) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var contentBuilder, thinkingBuilder strings.Builder
	for scanner.Scan() {
		var line map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if msg, ok := line["message"].(map[string]interface{}); ok {
			if c := extractContentText(msg["content"]); c != "" {
				contentBuilder.WriteString(c)
			}
			if t := extractThinkingText(msg["content"]); t != "" {
				thinkingBuilder.WriteString(t)
			}
		}
		if isErr, _ := line["is_error"].(bool); isErr {
			if res, ok := line["result"].(string); ok {
				cliErr = res
			}
		}
	}
	return contentBuilder.String(), thinkingBuilder.String(), cliErr
}
