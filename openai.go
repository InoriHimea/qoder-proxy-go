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

type ChatRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Stream          bool             `json:"stream"`
	MaxTokens       int              `json:"max_tokens"`
	ReasoningEffort string           `json:"reasoning_effort"`
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type ChatChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
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
		// Convert Anthropic to OpenAI-style ChatRequest for our simple DirectClient
		// Or implement HandleAnthropic in DirectClient. For now, we skip it or use CLI.
		AddSystemLog("Direct API requested but not yet fully implemented for Anthropic protocol. Falling back to CLI.", "warn", "direct")
	}

	prompt := anthropicMessagesToPrompt(req)
	opts := SpawnOptions{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		MaxTokens:       req.MaxTokens,
	}

	stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
	if err != nil {
		ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
		um.Record(req.Model, len(prompt), 0, true, time.Since(started).Milliseconds())
		return
	}

	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	if !req.Stream {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)
		
		var fullContent strings.Builder
		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
				if msg, ok := line["message"].(map[string]interface{}); ok {
					if content := extractContentText(msg["content"]); content != "" {
						fullContent.WriteString(content)
					}
				}
			}
		}
		
		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("Scanner error in anthropic non-stream: %v", err), "error", "cli")
		}
		
		outStr := fullContent.String()
		respData := buildAnthropicFullResponse(id, req.Model, outStr)
		ctx.SetUserValue("response_body", respData)
		json.NewEncoder(ctx).Encode(respData)
		um.Record(req.Model, len(prompt), len(outStr), false, time.Since(started).Milliseconds())
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

		writeEvent("message_start", buildAnthropicStartEvent(id, req.Model))
		writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: 0, ContentBlock: &AnthropicContent{Type: "text", Text: ""}})

		scanner := bufio.NewScanner(stdout)
		outLen := 0
		for scanner.Scan() {
			var line map[string]interface{}
			if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
				if line["type"] == "assistant" {
					if msg, ok := line["message"].(map[string]interface{}); ok {
						if content := extractContentText(msg["content"]); content != "" {
							outLen += len(content)
							writeEvent("content_block_delta", buildAnthropicDeltaEvent(content))
						}
					}
				}
			}
		}

		writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: 0})
		writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "end_turn"}})
		writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		
		um.Record(req.Model, len(prompt), outLen, false, time.Since(started).Milliseconds())
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
		dc.HandleChat(ctx, req, um)
		return
	}

	if req.Stream {
		ctx.SetUserValue("response_body", "[Streaming Response...]")
		handleChatCompletionsStream(ctx, req, cm, um, started)
		return
	}

	// Non-streaming logic with tool loop support
	workingMessages := req.Messages
	var finalContent string
	var finalParsedOutput *ParsedToolOutput
	
	maxDepth := 5
	for depth := 0; depth < maxDepth; depth++ {
		prompt := messagesToPrompt(workingMessages)
		opts := SpawnOptions{Model: req.Model, ReasoningEffort: req.ReasoningEffort, MaxTokens: req.MaxTokens}
		
		stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
		if err != nil {
			ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
			um.Record(req.Model, len(prompt), 0, true, time.Since(started).Milliseconds())
			return
		}
		
		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)
		
		var contentBuilder strings.Builder
		var cliErrorMsg string
		for scanner.Scan() {
			rawLine := scanner.Text()
			var line map[string]interface{}
			if err := json.Unmarshal([]byte(rawLine), &line); err == nil {
				if msg, ok := line["message"].(map[string]interface{}); ok {
					if content := extractContentText(msg["content"]); content != "" {
						contentBuilder.WriteString(content)
					}
				} else if line["type"] == "result" && line["subtype"] == "success" {
					if res, ok := line["result"].(string); ok && res != "" {
						contentBuilder.WriteString(res)
					}
				} else {

					// Check for explicit error in result
					if isErr, _ := line["is_error"].(bool); isErr {
						if res, ok := line["result"].(string); ok {
							cliErrorMsg = res
						}
					}
					// Log other types of output for debugging
					AddSystemLog(fmt.Sprintf("CLI output (non-message): %s", rawLine), "debug", "cli")
				}
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
			um.Record(req.Model, len(prompt), 0, true, time.Since(started).Milliseconds())
			return
		}


		finalContent = contentBuilder.String()
		finalParsedOutput = parseToolCallOutput(finalContent)

		if finalParsedOutput.Type != "tool_calls" {
			break
		}

		// Execute tools and append to working messages
		toolResults := []Message{}
		var openAiToolCalls []interface{}
		
		for _, tc := range finalParsedOutput.ToolCalls {
			openAiToolCalls = append(openAiToolCalls, map[string]interface{}{
				"id": tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name": tc.Function.Name,
					"arguments": tc.Function.Arguments,
				},
			})
			
			res := executeToolCall(tc)
			b, _ := json.Marshal(res)
			toolResults = append(toolResults, Message{
				Role: "tool",
				Content: string(b),
			})
		}
		
		workingMessages = append(workingMessages, Message{
			Role: "assistant",
			Content: finalParsedOutput.PrefixText,
		})
		
		// To adhere strictly to OpenAI spec, tool_calls must be injected here but Go typing makes it tricky.
		// For the proxy layer loop, we just need to append the raw results to feed back.
		workingMessages = append(workingMessages, toolResults...)
	}

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	
	msgMap := map[string]interface{}{
		"role": "assistant",
		"content": finalContent,
	}
	
	if finalParsedOutput != nil && finalParsedOutput.Type == "tool_calls" {
		msgMap["content"] = finalParsedOutput.PrefixText
		var tcs []interface{}
		for _, tc := range finalParsedOutput.ToolCalls {
			tcs = append(tcs, map[string]interface{}{
				"id": tc.ID,
				"type": "function",
				"function": map[string]interface{}{
					"name": tc.Function.Name,
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
				"index": 0,
				"message": msgMap,
				"finish_reason": "stop",
			},
		},
	}
	
	if finalParsedOutput != nil && finalParsedOutput.Type == "tool_calls" {
		resp["choices"].([]map[string]interface{})[0]["finish_reason"] = "tool_calls"
	}
	
	json.NewEncoder(ctx).Encode(resp)
	um.Record(req.Model, len(messagesToPrompt(req.Messages)), len(finalContent), false, time.Since(started).Milliseconds())
}

func handleChatCompletionsStream(ctx *fasthttp.RequestCtx, req ChatRequest, cm *ConfigManager, um *UsageManager, started time.Time) {
	prompt := messagesToPrompt(req.Messages)
	opts := SpawnOptions{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		MaxTokens:       req.MaxTokens,
	}

	stdout, err := spawnQoderCli(ctx, prompt, opts, cm)
	if err != nil {
		ctx.Error(RedactSensitiveInfo(fmt.Sprintf("Spawn failed: %v", err)), http.StatusInternalServerError)
		um.Record(req.Model, len(prompt), 0, true, time.Since(started).Milliseconds())
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

	outLen := 0
	var fullContent strings.Builder
	for scanner.Scan() {
		var line map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &line); err == nil {
			if line["type"] == "assistant" {
				if msg, ok := line["message"].(map[string]interface{}); ok {
					if content := extractContentText(msg["content"]); content != "" {
						outLen += len(content)
						fullContent.WriteString(content)
						chunk := ChatChunk{
							ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						}
						chunk.Choices = []struct {
							Index int `json:"index"`
							Delta struct {
								Role    string `json:"role,omitempty"`
								Content string `json:"content,omitempty"`
							} `json:"delta"`
							FinishReason *string `json:"finish_reason"`
						}{
							{
								Index: 0,
							},
						}
						chunk.Choices[0].Delta.Content = content

						data, _ := json.Marshal(chunk)
						fmt.Fprintf(w, "data: %s\n\n", data)
						w.Flush()
					}
				}
			} else if line["type"] == "result" && line["subtype"] == "success" {
				if res, ok := line["result"].(string); ok && res != "" {
					outLen += len(res)
					fullContent.WriteString(res)
					chunk := ChatChunk{
						ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
					}
					chunk.Choices = []struct {
						Index int `json:"index"`
						Delta struct {
							Role    string `json:"role,omitempty"`
							Content string `json:"content,omitempty"`
						} `json:"delta"`
						FinishReason *string `json:"finish_reason"`
					}{
						{
							Index: 0,
						},
					}
					chunk.Choices[0].Delta.Content = res

					data, _ := json.Marshal(chunk)
					fmt.Fprintf(w, "data: %s\n\n", data)
					w.Flush()
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

	fmt.Fprintf(w, "data: [DONE]\n\n")
	w.Flush()

	if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
		UpdateRequestLogResponse(logID, map[string]interface{}{"streamed_content": fullContent.String()})
	}

	um.Record(req.Model, len(prompt), outLen, false, time.Since(started).Milliseconds())
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

func messagesToPrompt(messages []Message) string {
	var sb strings.Builder
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
		if content != "" {
			sb.WriteString(fmt.Sprintf("%s: %s\n\n", strings.Title(m.Role), content))
		}
	}
	return sb.String()
}
