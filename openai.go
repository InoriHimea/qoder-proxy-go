package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

type ChatRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       int             `json:"max_tokens"`
	ReasoningEffort string          `json:"reasoning_effort"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	ToolChoice      interface{}     `json:"tool_choice,omitempty"`
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
	// ToolCalls carries an assistant turn's prior tool_calls (round-tripped
	// history); ToolCallID links a `role: tool` result back to the call it
	// answers. Both are needed to reconstruct multi-turn tool conversations
	// as text the CLI can read, since the CLI never executes tools itself.
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ChatChunkDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []interface{} `json:"tool_calls,omitempty"`
}

// strPtr returns a pointer to s, used for the optional finish_reason field.
func strPtr(s string) *string { return &s }

type ChatChunkChoice struct {
	Index int            `json:"index"`
	Delta ChatChunkDelta `json:"delta"`
	// Message is populated instead of Delta on some CN gateway chunks
	// (mirrors the real client's `delta ?? message` fallback).
	Message      ChatChunkDelta `json:"message"`
	FinishReason *string        `json:"finish_reason"`
}

// Content returns the choice's text, preferring Delta.Content and falling
// back to Message.Content (CN gateway sends either shape).
func (c ChatChunkChoice) Content() string {
	if c.Delta.Content != "" {
		return c.Delta.Content
	}
	return c.Message.Content
}

// ReasoningContent returns the choice's reasoning text, preferring
// Delta.ReasoningContent and falling back to Message.ReasoningContent.
func (c ChatChunkChoice) ReasoningContent() string {
	if c.Delta.ReasoningContent != "" {
		return c.Delta.ReasoningContent
	}
	return c.Message.ReasoningContent
}

type ChatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []ChatChunkChoice `json:"choices"`
}

func extractContentText(content interface{}) string {
	if s, ok := content.(string); ok {
		return s
	}
	if m, ok := content.(map[string]interface{}); ok {
		if text, ok := m["text"].(string); ok {
			return text
		}
		if text, ok := m["content"].(string); ok {
			return text
		}
		if text, ok := m["reasoning_content"].(string); ok {
			return text
		}
	}
	if arr, ok := content.([]interface{}); ok {
		var sb strings.Builder
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "thinking" {
					continue
				}
				if text, ok := m["text"].(string); ok {
					sb.WriteString(text)
				} else if text, ok := m["content"].(string); ok {
					sb.WriteString(text)
				}
			} else if s, ok := item.(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

// extractThinkingText pulls the CLI's {"type":"thinking","thinking":"..."}
// content blocks out of a stream-json `message.content` array, mirroring
// extractContentText's shape but collecting only reasoning text.
func extractThinkingText(content interface{}) string {
	arr, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "thinking" {
			continue
		}
		if text, ok := m["thinking"].(string); ok {
			sb.WriteString(text)
		}
	}
	return sb.String()
}

// mergeSystemPrompt appends the tool-protocol prompt to the caller's system
// prompt, separated by a blank line. Returns sys unchanged when toolPrompt is
// empty (no tools requested).
func mergeSystemPrompt(sys, toolPrompt string) string {
	if toolPrompt == "" {
		return sys
	}
	if sys == "" {
		return toolPrompt
	}
	return sys + "\n\n" + toolPrompt
}

func handleAnthropicMessages(ctx *fasthttp.RequestCtx, cm *ConfigManager, um *UsageManager, dc *DirectClient) {
	started := time.Now()
	var req AnthropicRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		ctx.Error("Invalid JSON", http.StatusBadRequest)
		um.Record("unknown", 0, 0, true, time.Since(started).Milliseconds())
		return
	}

	cfg := cm.Get()
	if cfg.UseDirectAPI {
		AddSystemLog("Direct API requested but not yet fully implemented for Anthropic protocol. Falling back to CLI.", "warn", "direct")
	}

	// Inject a format-only system prompt describing available Anthropic tools.
	// Like the OpenAI path, the prompt contains no role-defining statements and
	// teaches the model the \`<tool_calls>\` JSON code-block shape to emit.
	var toolPrompt string
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, true)
	}

	normalizedSystem := normalizeAnthropicSystem(req.System)
	fullSystem := mergeSystemPrompt(normalizedSystem, toolPrompt)

	prompt := anthropicMessagesToPrompt(req)
	AddSystemLog(fmt.Sprintf("Anthropic system length: %d, Prompt length: %d", len(fullSystem), len(prompt)), "info", "cli")
	opts := SpawnOptions{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		MaxTokens:       req.MaxTokens,
		SystemPrompt:    fullSystem,
		DisableTools:    true,
	}

	stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
	if err != nil {
		ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
		um.Record(req.Model, countTokens(prompt), 0, true, time.Since(started).Milliseconds())
		return
	}

	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	if !req.Stream {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)

		var fullContent strings.Builder
		var fullThinking strings.Builder
		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
				if msg, ok := line["message"].(map[string]interface{}); ok {
					if content := extractContentText(msg["content"]); content != "" {
						fullContent.WriteString(content)
					}
					if thinking := extractThinkingText(msg["content"]); thinking != "" {
						fullThinking.WriteString(thinking)
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("Scanner error in anthropic non-stream: %v", err), "error", "cli")
		}

		outStr := fullContent.String()
		thinkingStr := fullThinking.String()
		inputTokens := countTokens(fullSystem + "\n" + prompt)
		respData := buildAnthropicResponse(id, req.Model, outStr, parseToolCallOutput(outStr), inputTokens, thinkingStr)
		ctx.SetUserValue("response_body", respData)
		json.NewEncoder(ctx).Encode(respData)
		um.Record(req.Model, countTokens(prompt), countTokens(thinkingStr+outStr), false, time.Since(started).Milliseconds())
		return
	}

	// Anthropic Streaming
	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer stdout.Close()
		writeEvent := func(eventType string, data interface{}) {
			d, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, d)
			w.Flush()
		}

		// The CLI emits whole JSON objects per line — buffer the whole stream to
		// decide whether to emit a text or tool_use reply, then replay.
		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)

		var fullContent strings.Builder
		var fullThinking strings.Builder
		var fragments []string
		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
				if line["type"] == "assistant" {
					if msg, ok := line["message"].(map[string]interface{}); ok {
						if content := extractContentText(msg["content"]); content != "" {
							fullContent.WriteString(content)
							fragments = append(fragments, content)
						}
						if thinking := extractThinkingText(msg["content"]); thinking != "" {
							fullThinking.WriteString(thinking)
						}
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("Scanner error in anthropic stream: %v", err), "error", "cli")
		}

		inputTokens := countTokens(fullSystem + "\n" + prompt)
		thinkingStr := fullThinking.String()
		writeEvent("message_start", buildAnthropicStartEvent(id, req.Model, inputTokens))

		nextIndex := 0
		if thinkingStr != "" {
			writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: nextIndex, ContentBlock: &AnthropicContent{Type: "thinking", Thinking: ""}})
			writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: nextIndex, Delta: &AnthropicEventDelta{Type: "thinking_delta", Thinking: thinkingStr}})
			writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: nextIndex})
			nextIndex++
		}

		parsed := parseToolCallOutput(fullContent.String())
		if parsed != nil && parsed.Type == "tool_calls" {
			if parsed.PrefixText != "" {
				writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: nextIndex, ContentBlock: &AnthropicContent{Type: "text", Text: ""}})
				writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: nextIndex, Delta: &AnthropicEventDelta{Type: "text_delta", Text: parsed.PrefixText}})
				writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: nextIndex})
				nextIndex++
			}
			for _, tc := range parsed.ToolCalls {
				blockIndex := nextIndex
				nextIndex++
				var input interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
					input = map[string]interface{}{}
				}
				writeEvent("content_block_start", AnthropicSSEEvent{
					Type:         "content_block_start",
					Index:        blockIndex,
					ContentBlock: &AnthropicContent{Type: "tool_use", ID: generateCallId("toolu_"), Name: tc.Function.Name, Input: input},
				})
				writeEvent("content_block_delta", AnthropicSSEEvent{
					Type:  "content_block_delta",
					Index: blockIndex,
					Delta: &AnthropicEventDelta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments},
				})
				writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: blockIndex})
			}
			writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "tool_use"}})
			writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		} else {
			writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: nextIndex, ContentBlock: &AnthropicContent{Type: "text", Text: ""}})
			for _, frag := range fragments {
				writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: nextIndex, Delta: &AnthropicEventDelta{Type: "text_delta", Text: frag}})
			}
			writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: nextIndex})
			writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "end_turn"}})
			writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		}

		um.Record(req.Model, inputTokens, countTokens(thinkingStr+fullContent.String()), false, time.Since(started).Milliseconds())
	})
}

func handleChatCompletions(ctx *fasthttp.RequestCtx, cm *ConfigManager, um *UsageManager, dc *DirectClient) {
	started := time.Now()
	var req ChatRequest
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		ctx.Error("Invalid JSON", http.StatusBadRequest)
		um.Record("unknown", 0, 0, true, time.Since(started).Milliseconds())
		return
	}

	cfg := cm.Get()
	if cfg.UseDirectAPI {
		AddSystemLog(fmt.Sprintf("Using Direct API for %s", req.Model), "info", "direct")
		fallback := dc.HandleChat(ctx, req, um)
		if !fallback {
			return
		}
		AddSystemLog("Direct API returned 401/404, falling back to CLI mode automatically...", "info", "system")
	}

	// Inject a format-only system prompt describing available tools.  The
	// prompt contains no role-defining statements — it only teaches the model
	// what tools exist and what JSON code-block shape to emit.
	var toolPrompt string
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, false)
	}

	if req.Stream {
		ctx.SetUserValue("response_body", "[Streaming Response...]")
		handleChatCompletionsStream(ctx, req, cm, um, started, toolPrompt)
		return
	}

	// Non-streaming logic
	sys, prompt := extractSystemAndPrompt(req.Messages)
	AddSystemLog(fmt.Sprintf("System prompt length: %d, Prompt length: %d", len(sys), len(prompt)), "info", "cli")
	opts := SpawnOptions{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		MaxTokens:       req.MaxTokens,
		SystemPrompt:    mergeSystemPrompt(sys, toolPrompt),
		DisableTools:    true,
	}

	stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
	if err != nil {
		ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
		um.Record(req.Model, countTokens(prompt), 0, true, time.Since(started).Milliseconds())
		return
	}

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 10*1024*1024)

	var contentBuilder strings.Builder
	var thinkingBuilder strings.Builder
	var cliErrorMsg string
	for scanner.Scan() {
		rawLine := scanner.Text()
		var line map[string]interface{}
		if err := json.Unmarshal([]byte(rawLine), &line); err == nil {
			if msg, ok := line["message"].(map[string]interface{}); ok {
				if content := extractContentText(msg["content"]); content != "" {
					contentBuilder.WriteString(content)
				}
				if thinking := extractThinkingText(msg["content"]); thinking != "" {
					thinkingBuilder.WriteString(thinking)
				}
			}

			// Check for explicit error in result
			if isErr, _ := line["is_error"].(bool); isErr {
				if res, ok := line["result"].(string); ok {
					cliErrorMsg = res
				}
			}
			// Log other types of output for debugging
			AddSystemLog(fmt.Sprintf("CLI output (non-message): %s", rawLine), "debug", "cli")
		} else {
			AddSystemLog(fmt.Sprintf("Failed to parse CLI output line: %s", rawLine), "warn", "cli")
		}
	}

	if err := scanner.Err(); err != nil {
		AddSystemLog(fmt.Sprintf("Scanner error in chat non-stream: %v", err), "error", "cli")
	}

	stdout.Close()

	if cliErrorMsg != "" {
		ctx.Error(fmt.Sprintf("CLI Error: %s", cliErrorMsg), http.StatusUnauthorized)
		um.Record(req.Model, countTokens(prompt), 0, true, time.Since(started).Milliseconds())
		return
	}

	finalContent := contentBuilder.String()
	finalThinking := thinkingBuilder.String()
	finalParsedOutput := parseToolCallOutput(finalContent)

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	msgMap := map[string]interface{}{
		"role":    "assistant",
		"content": finalContent,
	}
	if finalThinking != "" {
		msgMap["reasoning_content"] = finalThinking
	}

	if finalParsedOutput != nil && finalParsedOutput.Type == "tool_calls" {
		msgMap["content"] = finalParsedOutput.PrefixText
		var tcs []interface{}
		for _, tc := range finalParsedOutput.ToolCalls {
			tcs = append(tcs, map[string]interface{}{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
		}
		msgMap["tool_calls"] = tcs
	}

	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msgMap,
				"finish_reason": "stop",
			},
		},
	}

	if finalParsedOutput != nil && finalParsedOutput.Type == "tool_calls" {
		resp["choices"].([]map[string]interface{})[0]["finish_reason"] = "tool_calls"
	}

	json.NewEncoder(ctx).Encode(resp)
	um.Record(req.Model, countTokens(prompt), countTokens(finalThinking+finalContent), false, time.Since(started).Milliseconds())
}

func handleChatCompletionsStream(ctx *fasthttp.RequestCtx, req ChatRequest, cm *ConfigManager, um *UsageManager, started time.Time, toolPrompt string) {
	sys, prompt := extractSystemAndPrompt(req.Messages)
	opts := SpawnOptions{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		MaxTokens:       req.MaxTokens,
		SystemPrompt:    mergeSystemPrompt(sys, toolPrompt),
		DisableTools:    true,
	}

	stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
	if err != nil {
		ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
		um.Record(req.Model, countTokens(prompt), 0, true, time.Since(started).Milliseconds())
		return
	}

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		// Increase buffer size to 10MB to handle large lines
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)

		// The CLI emits whole JSON objects per line, not token-level deltas, so
		// a single line may itself be the entire `{"tool_calls": [...]}`
		// payload (or a fragment of one spread across lines). We can't know
		// which until the process finishes, so fragments are buffered and only
		// replayed as content deltas once we've confirmed the assembled output
		// is plain text; a tool-call payload is instead emitted as one
		// tool_calls chunk.
		var fullContent strings.Builder
		var fullThinking strings.Builder
		var fragments []string
		var thinkingFragments []string
		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
				if msg, ok := line["message"].(map[string]interface{}); ok {
					if content := extractContentText(msg["content"]); content != "" {
						fullContent.WriteString(content)
						fragments = append(fragments, content)
					}
					if thinking := extractThinkingText(msg["content"]); thinking != "" {
						fullThinking.WriteString(thinking)
						thinkingFragments = append(thinkingFragments, thinking)
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("Scanner error in chat stream: %v", err), "error", "cli")
			// If it's a buffer too long error, we should definitely know
			if err == bufio.ErrTooLong {
				AddSystemLog("Line too long for scanner buffer (10MB)", "error", "cli")
			}
		}

		sendChunk := func(delta ChatChunkDelta, finishReason *string) {
			chunk := ChatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model}
			chunk.Choices = []ChatChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.Flush()
		}

		for _, frag := range thinkingFragments {
			sendChunk(ChatChunkDelta{ReasoningContent: frag}, nil)
		}

		parsed := parseToolCallOutput(fullContent.String())
		if parsed != nil && parsed.Type == "tool_calls" {
			if parsed.PrefixText != "" {
				sendChunk(ChatChunkDelta{Content: parsed.PrefixText}, nil)
			}
			var tcs []interface{}
			for i, tc := range parsed.ToolCalls {
				tcs = append(tcs, map[string]interface{}{
					"index": i,
					"id":    tc.ID,
					"type":  "function",
					"function": map[string]interface{}{
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					},
				})
			}
			sendChunk(ChatChunkDelta{ToolCalls: tcs}, strPtr("tool_calls"))
		} else {
			for _, frag := range fragments {
				sendChunk(ChatChunkDelta{Content: frag}, nil)
			}
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.Flush()

		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			UpdateRequestLogResponse(logID, map[string]interface{}{"streamed_content": fullContent.String()})
		}

		um.Record(req.Model, countTokens(prompt), countTokens(fullThinking.String()+fullContent.String()), false, time.Since(started).Milliseconds())
	})

}

func handleModels(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	cfg := cm.Get()

	// If the request comes from the dashboard UI, return the full model structs
	if string(ctx.Path()) == "/dashboard/api/models" {
		resp := map[string]interface{}{
			"models": cfg.Models,
		}
		json.NewEncoder(ctx).Encode(resp)
		return
	}

	// OpenAI Compatible Response
	data := make([]map[string]interface{}, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		data = append(data, map[string]interface{}{
			"id":       m.ID,
			"object":   "model",
			"created":  1677610602,
			"owned_by": "qoder",
		})
	}
	resp := map[string]interface{}{
		"object": "list",
		"data":   data,
	}
	json.NewEncoder(ctx).Encode(resp)
}

func extractSystemAndPrompt(messages []Message) (string, string) {
	var sysSb strings.Builder
	var userSb strings.Builder
	// Collect tool round-trips from user/tool/assistant messages and stitch
	// them as <tool_use>/<tool_result> tagged text.
	var toolBlocks []string
	for _, m := range messages {
		content := ""
		switch v := m.Content.(type) {
		case string:
			content = v
		case []interface{}:
			for _, part := range v {
				if p, ok := part.(map[string]interface{}); ok {
					if t, ok := p["text"].(string); ok {
						content += t
					}
				}
			}
		}
		switch strings.ToLower(m.Role) {
		case "system":
			if content != "" {
				sysSb.WriteString(content + "\n\n")
			}
		case "tool":
			// Result of a prior tool call: formatToolResultForPrompt handles
			// the content shape — a plain string, or a [{type:"text"...}] array.
			rid := m.ToolCallID
			if rid == "" {
				rid = "unknown"
			}
			rendered := ""
			switch v := m.Content.(type) {
			case string:
				rendered = v
			case []interface{}:
				for _, part := range v {
					if p, ok := part.(map[string]interface{}); ok {
						if t, ok := p["text"].(string); ok {
							rendered += t
						}
					} else if s, ok := part.(string); ok {
						rendered += s
					}
				}
			default:
				if v != nil {
					rendered = fmt.Sprintf("%v", v)
				}
			}
			toolBlocks = append(toolBlocks, fmt.Sprintf("<tool_result id=\"%s\">\n%s\n</tool_result>", rid, rendered))
		case "assistant":
			if content != "" {
				userSb.WriteString(fmt.Sprintf("%s: %s\n\n", strings.Title(m.Role), content))
			}
			// Prior tool_calls history (assistant wanted to call a tool).
			for _, tc := range m.ToolCalls {
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				toolBlocks = append(toolBlocks,
					fmt.Sprintf("<tool_use name=\"%s\" id=\"%s\">\n%s\n</tool_use>",
						tc.Function.Name, tc.ID, args))
			}
		default:
			if content != "" {
				userSb.WriteString(fmt.Sprintf("%s: %s\n\n", strings.Title(m.Role), content))
			}
		}
	}
	if len(toolBlocks) > 0 {
		userSb.WriteString(strings.Join(toolBlocks, "\n\n") + "\n\n")
	}
	return strings.TrimSpace(sysSb.String()), strings.TrimSpace(userSb.String())
}

func handleModelsRefresh(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	cfg := cm.Get()
	binaryName := "qodercli"
	backendName := "global"
	if strings.ToLower(cfg.Backend) == "cn" {
		binaryName = "qoderclicn"
		backendName = "cn"
	}
	cmdPath := binaryName
	if runtime.GOOS == "windows" {
		cmdPath = binaryName + ".cmd"
	}

	if cfg.Token != "" {
		if err := writeDeviceTokenToKeychain(cfg.Token, cfg.UserID, cfg.RefreshToken, cfg.ExpireTime, backendName); err != nil {
			AddSystemLog(fmt.Sprintf("Failed to write device token before models refresh: %v", err), "error", "spawn")
		}
	}

	cmd := exec.Command(cmdPath, "chat", "--list-models")
	cmd.Dir = os.TempDir()
	cmd.Env = os.Environ()
	if cfg.Token != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("QODER_API_KEY=%s", cfg.Token))
	}
	cmd.Env = append(cmd.Env, "NO_BROWSER=1", "CI=1")

	out, err := cmd.CombinedOutput()
	if err != nil {
		AddSystemLog(fmt.Sprintf("Models refresh failed (%s): %v: %s", cmdPath, err, string(out)), "error", "cli")
		ctx.Error(fmt.Sprintf("Failed to run %s: %v: %s", binaryName, err, string(out)), 500)
		return
	}

	lines := strings.Split(string(out), "\n")
	var newModels []Model
	seen := make(map[string]bool)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line == "MODEL" || strings.Contains(line, "Warning:") {
			continue
		}
		if !seen[line] {
			seen[line] = true
			tier := "paid"
			if strings.Contains(line, "Flash") || line == "Auto" {
				tier = "free"
			}
			newModels = append(newModels, Model{
				ID:          line,
				Label:       line,
				Tier:        tier,
				Description: "Auto-detected model",
			})
		}
	}

	if len(newModels) > 0 {
		cfg := cm.Get()
		cfg.Models = newModels
		cm.Update(cfg)
	}

	json.NewEncoder(ctx).Encode(map[string]interface{}{"ok": true, "models": newModels})
}
