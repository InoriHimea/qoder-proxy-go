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
		if strings.ToLower(cfg.Backend) == "cn" {
			AddSystemLog(fmt.Sprintf("Using Direct API (CN) for Anthropic protocol, model %s", req.Model), "info", "direct")
			if !dc.HandleAnthropicChat(ctx, req, um) {
				return
			}
			AddSystemLog("Direct API (CN) failed, falling back to CLI mode automatically...", "info", "system")
		} else {
			AddSystemLog("Direct API requested but not implemented for Anthropic protocol on this backend. Falling back to CLI.", "warn", "direct")
		}
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

	promptTokens := countTokens(fullSystem + "\n" + prompt)
	if limit := modelContextWindow(cfg.Models, req.Model); limit > 0 && promptTokens > limit {
		AddSystemLog(fmt.Sprintf("Context length %d exceeds model %s limit %d, rejecting", promptTokens, req.Model, limit), "warn", "context")
		ctx.SetStatusCode(http.StatusRequestEntityTooLarge)
		ctx.SetBodyString(fmt.Sprintf("Request context (%d tokens) exceeds model %s context window (%d tokens). Please reduce conversation length or use a model with a larger context window.", promptTokens, req.Model, limit))
		um.Record(req.Model, promptTokens, 0, true, time.Since(started).Milliseconds())
		return
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
		ctx.SetUserValue("log_metrics", &LogMetrics{
			InputTokens:    respData.Usage.InputTokens,
			OutputTokens:   respData.Usage.OutputTokens,
			ThinkingTokens: countTokens(thinkingStr),
		})
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

		// True streaming: forward each CLI stdout line's extracted text as soon
		// as it arrives instead of buffering the whole process output first.
		// message_start only depends on the already-known prompt/system, so it
		// can be sent immediately — this is what gets the client its first byte
		// well before the CLI finishes, avoiding the idle-timeout disconnects
		// long agentic turns used to trigger.
		inputTokens := countTokens(fullSystem + "\n" + prompt)
		writeEvent("message_start", buildAnthropicStartEvent(id, req.Model, inputTokens))

		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)

		var fullContent strings.Builder
		var fullThinking strings.Builder
		classifier := &streamClassifier{}

		nextIndex := 0
		thinkingOpen, thinkingIndex := false, 0
		textOpen, textIndex := false, 0
		var resolvedTools *ParsedToolOutput

		closeThinkingIfOpen := func() {
			if thinkingOpen {
				writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: idxPtr(thinkingIndex)})
				thinkingOpen = false
			}
		}
		openTextIfNeeded := func() {
			if !textOpen {
				textIndex = nextIndex
				nextIndex++
				writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: idxPtr(textIndex), ContentBlock: &AnthropicContent{Type: "text", Text: ""}})
				textOpen = true
			}
		}

		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line["type"] != "assistant" {
				continue
			}
			msg, ok := line["message"].(map[string]interface{})
			if !ok {
				continue
			}

			if resolvedTools != nil {
				// A tool_calls payload was already resolved and its blocks
				// closed out; keep draining to EOF so the CLI subprocess never
				// blocks on a full stdout pipe, but stop forwarding/parsing.
				if content := extractContentText(msg["content"]); content != "" {
					fullContent.WriteString(content)
				}
				if thinking := extractThinkingText(msg["content"]); thinking != "" {
					fullThinking.WriteString(thinking)
				}
				continue
			}

			if thinking := extractThinkingText(msg["content"]); thinking != "" {
				fullThinking.WriteString(thinking)
				if !thinkingOpen {
					thinkingIndex = nextIndex
					nextIndex++
					writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: idxPtr(thinkingIndex), ContentBlock: &AnthropicContent{Type: "thinking", Thinking: ""}})
					thinkingOpen = true
				}
				writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: idxPtr(thinkingIndex), Delta: &AnthropicEventDelta{Type: "thinking_delta", Thinking: thinking}})
			}

			content := extractContentText(msg["content"])
			if content == "" {
				continue
			}
			fullContent.WriteString(content)

			plainText, resolved := classifier.feed(content)
			if plainText != "" {
				closeThinkingIfOpen()
				openTextIfNeeded()
				writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: idxPtr(textIndex), Delta: &AnthropicEventDelta{Type: "text_delta", Text: plainText}})
			}

			if resolved != nil {
				closeThinkingIfOpen()
				if textOpen {
					writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: idxPtr(textIndex)})
					textOpen = false
				}
				for _, tc := range resolved.ToolCalls {
					blockIndex := nextIndex
					nextIndex++
					var input interface{}
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
						input = map[string]interface{}{}
					}
					writeEvent("content_block_start", AnthropicSSEEvent{
						Type:         "content_block_start",
						Index:        idxPtr(blockIndex),
						ContentBlock: &AnthropicContent{Type: "tool_use", ID: generateCallId("toolu_"), Name: tc.Function.Name, Input: input},
					})
					writeEvent("content_block_delta", AnthropicSSEEvent{
						Type:  "content_block_delta",
						Index: idxPtr(blockIndex),
						Delta: &AnthropicEventDelta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments},
					})
					writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: idxPtr(blockIndex)})
				}
				resolvedTools = resolved
				// Do not break — keep draining the scanner to EOF below.
			}
		}

		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("Scanner error in anthropic stream: %v", err), "error", "cli")
		}

		thinkingStr := fullThinking.String()

		if resolvedTools == nil {
			if parsed := parseToolCallOutput(fullContent.String()); parsed.Type == "tool_calls" {
				resolvedTools = parsed
			}
		}

		if resolvedTools == nil {
			if remaining := classifier.flush(); remaining != "" {
				if parsed := parseToolCallOutput(remaining); parsed.Type == "tool_calls" {
					resolvedTools = parsed
				} else {
					closeThinkingIfOpen()
					openTextIfNeeded()
					writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: idxPtr(textIndex), Delta: &AnthropicEventDelta{Type: "text_delta", Text: parsed.PrefixText}})
				}
			}
			closeThinkingIfOpen()
			openTextIfNeeded()
			writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: idxPtr(textIndex)})
			writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "end_turn", Usage: &AnthropicDeltaUsage{OutputTokens: countTokens(thinkingStr + fullContent.String())}}})
			writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		} else {
			writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "tool_use", Usage: &AnthropicDeltaUsage{OutputTokens: countTokens(thinkingStr + fullContent.String())}}})
			writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		}

		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			parsed := resolvedTools
			if parsed == nil {
				parsed = parseToolCallOutput(fullContent.String())
			}
			respData := buildAnthropicResponse(id, req.Model, fullContent.String(), parsed, inputTokens, thinkingStr)
			UpdateRequestLogResponse(logID, respData)
		}

		um.Record(req.Model, inputTokens, countTokens(thinkingStr+fullContent.String()), false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{
			InputTokens:    inputTokens,
			OutputTokens:   countTokens(thinkingStr + fullContent.String()),
			ThinkingTokens: countTokens(thinkingStr),
		})
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

	// Inject a format-only system prompt describing available tools.  The
	// prompt contains no role-defining statements — it only teaches the model
	// what tools exist and what JSON code-block shape to emit.
	var toolPrompt string
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, false)
	}

	if cfg.UseDirectAPI {
		AddSystemLog(fmt.Sprintf("Using Direct API for %s", req.Model), "info", "direct")
		// Clone the request so tool-prompt injection doesn't mutate the caller's
		// messages array (important: direct API needs tool definitions baked into
		// the system message, unlike the CLI path which adds it separately).
		directReq := req
		if toolPrompt != "" {
			directReq.Messages = make([]Message, len(req.Messages))
			copy(directReq.Messages, req.Messages)
			// Prepend tool prompt to the existing system message (if any),
			// or inject a new system message at the top.
			foundSys := false
			for i := range directReq.Messages {
				if strings.ToLower(directReq.Messages[i].Role) == "system" {
					if s, ok := directReq.Messages[i].Content.(string); ok {
						directReq.Messages[i].Content = s + "\n\n" + toolPrompt
					}
					foundSys = true
					break
				}
			}
			if !foundSys {
				directReq.Messages = append([]Message{{Role: "system", Content: toolPrompt}}, directReq.Messages...)
			}
		}
		fallback := dc.HandleChat(ctx, directReq, um)
		if !fallback {
			return
		}
		AddSystemLog("Direct API returned 401/404, falling back to CLI mode automatically...", "info", "system")
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

	promptTokens := countTokens(opts.SystemPrompt + "\n" + prompt)
	if limit := modelContextWindow(cfg.Models, req.Model); limit > 0 && promptTokens > limit {
		AddSystemLog(fmt.Sprintf("Context length %d exceeds model %s limit %d, rejecting", promptTokens, req.Model, limit), "warn", "context")
		ctx.SetStatusCode(http.StatusRequestEntityTooLarge)
		ctx.SetBodyString(fmt.Sprintf("Request context (%d tokens) exceeds model %s context window (%d tokens). Please reduce conversation length or use a model with a larger context window.", promptTokens, req.Model, limit))
		um.Record(req.Model, promptTokens, 0, true, time.Since(started).Milliseconds())
		return
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

	ctx.SetUserValue("response_body", resp)
	json.NewEncoder(ctx).Encode(resp)
	um.Record(req.Model, countTokens(prompt), countTokens(finalThinking+finalContent), false, time.Since(started).Milliseconds())
	ctx.SetUserValue("log_metrics", &LogMetrics{
		InputTokens:    countTokens(prompt),
		OutputTokens:   countTokens(finalThinking + finalContent),
		ThinkingTokens: countTokens(finalThinking),
	})
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

	promptTokens := countTokens(opts.SystemPrompt + "\n" + prompt)
	cfg := cm.Get()
	if limit := modelContextWindow(cfg.Models, req.Model); limit > 0 && promptTokens > limit {
		AddSystemLog(fmt.Sprintf("Context length %d exceeds model %s limit %d, rejecting", promptTokens, req.Model, limit), "warn", "context")
		ctx.SetStatusCode(http.StatusRequestEntityTooLarge)
		ctx.SetBodyString(fmt.Sprintf("Request context (%d tokens) exceeds model %s context window (%d tokens). Please reduce conversation length or use a model with a larger context window.", promptTokens, req.Model, limit))
		um.Record(req.Model, promptTokens, 0, true, time.Since(started).Milliseconds())
		return
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

		sendChunk := func(delta ChatChunkDelta, finishReason *string) {
			chunk := ChatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model}
			chunk.Choices = []ChatChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.Flush()
		}

		// True streaming: forward each CLI stdout line's extracted text as soon
		// as it arrives. The CLI emits whole JSON objects per line, not
		// token-level deltas, so a single line may itself be the entire
		// `{"tool_calls": [...]}` payload (or a fragment of one spread across
		// lines) — streamClassifier holds candidate JSON text until it's
		// confirmed one way or the other, forwarding everything else as soon
		// as it's safe to.
		var fullContent strings.Builder
		var fullThinking strings.Builder
		classifier := &streamClassifier{}
		var resolvedTools *ParsedToolOutput

		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
				continue
			}
			msg, ok := line["message"].(map[string]interface{})
			if !ok {
				continue
			}

			if resolvedTools != nil {
				// Already emitted the tool_calls chunk; keep draining to EOF so
				// the CLI subprocess never blocks on a full stdout pipe, but
				// stop forwarding/parsing.
				if content := extractContentText(msg["content"]); content != "" {
					fullContent.WriteString(content)
				}
				if thinking := extractThinkingText(msg["content"]); thinking != "" {
					fullThinking.WriteString(thinking)
				}
				continue
			}

			if thinking := extractThinkingText(msg["content"]); thinking != "" {
				fullThinking.WriteString(thinking)
				sendChunk(ChatChunkDelta{ReasoningContent: thinking}, nil)
			}

			content := extractContentText(msg["content"])
			if content == "" {
				continue
			}
			fullContent.WriteString(content)

			plainText, resolved := classifier.feed(content)
			if plainText != "" {
				sendChunk(ChatChunkDelta{Content: plainText}, nil)
			}

			if resolved != nil {
				var tcs []interface{}
				for i, tc := range resolved.ToolCalls {
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
				resolvedTools = resolved
				// Do not break — keep draining the scanner to EOF below.
			}
		}

		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("Scanner error in chat stream: %v", err), "error", "cli")
			// If it's a buffer too long error, we should definitely know
			if err == bufio.ErrTooLong {
				AddSystemLog("Line too long for scanner buffer (10MB)", "error", "cli")
			}
		}

		if resolvedTools == nil {
			if remaining := classifier.flush(); remaining != "" {
				if parsed := parseToolCallOutput(remaining); parsed.Type == "tool_calls" {
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
					resolvedTools = parsed
				} else {
					sendChunk(ChatChunkDelta{Content: parsed.PrefixText}, nil)
				}
			}
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		w.Flush()

		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			UpdateRequestLogResponse(logID, map[string]interface{}{"streamed_content": fullContent.String()})
		}

		um.Record(req.Model, countTokens(prompt), countTokens(fullThinking.String()+fullContent.String()), false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{
			InputTokens:    countTokens(prompt),
			OutputTokens:   countTokens(fullThinking.String() + fullContent.String()),
			ThinkingTokens: countTokens(fullThinking.String()),
		})
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
			lowerLine := strings.ToLower(line)
			if strings.Contains(lowerLine, "flash") || lowerLine == "auto" ||
				strings.Contains(lowerLine, "38max") || strings.Contains(lowerLine, "free") {
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
