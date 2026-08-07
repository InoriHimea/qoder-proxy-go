package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

// ── Responses streaming emitter ─────────────────────────────────────────────
//
// Emits the OpenAI Responses SSE lifecycle with named `event:` lines and a
// monotonic `sequence_number` on every payload, and NO `data: [DONE]`
// sentinel (that sentinel is what breaks codex's responses parser). Sequence:
//
//   response.created → response.in_progress
//   → [reasoning]  output_item.added → reasoning_summary_part.added
//                  → reasoning_summary_text.delta* → .done
//                  → reasoning_summary_part.done → output_item.done
//   → [message]    output_item.added → content_part.added
//                  → output_text.delta* → output_text.done
//                  → content_part.done → output_item.done
//   → [each tool]  output_item.added → function_call_arguments.delta
//                  → .done → output_item.done
//   → response.completed        (error path: response.failed)
//
// It is driven identically to handleAnthropicStreamCN: a streamClassifier
// holds candidate tool-call JSON so it is never leaked as text, and once a
// tool payload resolves the driver stops forwarding but keeps draining the
// source to EOF.

type responsesEmitter struct {
	w           *bufio.Writer
	seq         int
	id          string
	model       string
	inputTokens int

	outputIndex int
	items       []ResponsesOutputItem

	reasoningOpen   bool
	reasoningItemID string

	msgOpen   bool
	msgItemID string
	msgText   strings.Builder

	rawContent   strings.Builder
	fullThinking strings.Builder

	nativeTools *ParsedToolOutput
}

func newResponsesEmitter(w *bufio.Writer, id, model string, inputTokens int) *responsesEmitter {
	return &responsesEmitter{w: w, id: id, model: model, inputTokens: inputTokens}
}

// nativeToolCallAccumulator joins fragmented native `delta.tool_calls` across
// streaming chunks. Arguments arrive one slice at a time keyed by the stable
// integer `index`; only the first fragment carries id + function.name. Later
// fragments may carry only a single `arguments` string with an empty function.
type nativeToolCallAccumulator struct {
	toolsByIndex          map[int]*ToolCall
	finishReasonToolCalls bool
}

func newNativeToolCallAccumulator() *nativeToolCallAccumulator {
	return &nativeToolCallAccumulator{toolsByIndex: map[int]*ToolCall{}}
}

func (a *nativeToolCallAccumulator) feed(choice ChatChunkChoice) {
	if len(choice.Delta.ToolCalls) == 0 && choice.FinishReason == nil {
		return
	}
	if len(choice.Delta.ToolCalls) == 0 {
		if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
			a.finishReasonToolCalls = true
		}
		return
	}
	a.finishReasonToolCalls = false
	for _, rawTC := range choice.Delta.ToolCalls {
		m, ok := rawTC.(map[string]interface{})
		if !ok {
			continue
		}
		idxFloat, _ := m["index"].(float64)
		idx := int(idxFloat)
		tc := a.toolsByIndex[idx]
		if tc == nil {
			tc = &ToolCall{}
			a.toolsByIndex[idx] = tc
		}
		if fn, ok := m["function"].(map[string]interface{}); ok && fn != nil {
			if name, ok := fn["name"].(string); ok && name != "" && tc.Function.Name == "" {
				tc.Function.Name = name
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				tc.Function.Arguments += args
			}
		}
		if id, ok := m["id"].(string); ok && id != "" && tc.ID == "" {
			tc.ID = id
		}
	}
	if choice.FinishReason != nil && *choice.FinishReason == "tool_calls" {
		a.finishReasonToolCalls = true
	}
}

// resolve returns the accumulated calls as a ParsedToolOutput. The empty case
// is nil so callers can distinguish "no tool calls seen" from "empty args".
func (a *nativeToolCallAccumulator) resolve() *ParsedToolOutput {
	if len(a.toolsByIndex) == 0 {
		return nil
	}
	ordered := make([]ToolCall, 0, len(a.toolsByIndex))
	for i := 0; i < len(a.toolsByIndex); i++ {
		if tc, ok := a.toolsByIndex[i]; ok {
			if tc.ID == "" {
				tc.ID = generateCallId("call_")
			}
			if tc.Function.Arguments == "" {
				tc.Function.Arguments = "{}"
			}
			if tc.Function.Name == "" {
				tc.Function.Name = "call_" + tc.ID
			}
			ordered = append(ordered, *tc)
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	return &ParsedToolOutput{Type: "tool_calls", ToolCalls: ordered}
}

// resolved reports true once the gateway has signalled tool_calls completion.
func (a *nativeToolCallAccumulator) resolved() bool {
	return a.finishReasonToolCalls
}

// result returns the accumulated ParsedToolOutput, consuming the accumulator.
func (a *nativeToolCallAccumulator) result() *ParsedToolOutput {
	out := a.resolve()
	a.toolsByIndex = nil
	a.finishReasonToolCalls = false
	return out
}

// emitResolvedToolCalls drains any pre-text accumulator into the output stream
// as a sequence of function_call items.
func (e *responsesEmitter) emitResolvedToolCalls(parsed *ParsedToolOutput) {
	if parsed == nil {
		return
	}
	e.closeReasoning()
	e.closeMessage()
	for _, tc := range parsed.ToolCalls {
		e.emitToolCall(tc)
	}
}

func (e *responsesEmitter) event(typ string, payload map[string]interface{}) {
	payload["type"] = typ
	payload["sequence_number"] = e.seq
	e.seq++
	d, _ := json.Marshal(payload)
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", typ, d)
	e.w.Flush()
}

func (e *responsesEmitter) snapshot(status string, output []ResponsesOutputItem, usage *ResponsesUsage, errObj *ResponsesError) ResponsesResponse {
	if output == nil {
		output = []ResponsesOutputItem{}
	}
	return ResponsesResponse{
		ID:        e.id,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    status,
		Model:     e.model,
		Output:    output,
		Usage:     usage,
		Error:     errObj,
	}
}

func (e *responsesEmitter) start() {
	resp := e.snapshot("in_progress", nil, nil, nil)
	e.event("response.created", map[string]interface{}{"response": resp})
	e.event("response.in_progress", map[string]interface{}{"response": resp})
}

// ── reasoning block ─────────────────────────────────────────────────────────

func (e *responsesEmitter) reasoningDelta(text string) {
	if !e.reasoningOpen {
		e.reasoningItemID = generateCallId("rs_")
		e.event("response.output_item.added", map[string]interface{}{
			"output_index": e.outputIndex,
			"item":         ResponsesOutputItem{Type: "reasoning", ID: e.reasoningItemID, Status: "in_progress", Summary: []ResponsesSummaryPart{}},
		})
		e.event("response.reasoning_summary_part.added", map[string]interface{}{
			"item_id": e.reasoningItemID, "output_index": e.outputIndex, "summary_index": 0,
			"part": ResponsesSummaryPart{Type: "summary_text", Text: ""},
		})
		e.reasoningOpen = true
	}
	e.fullThinking.WriteString(text)
	e.event("response.reasoning_summary_text.delta", map[string]interface{}{
		"item_id": e.reasoningItemID, "output_index": e.outputIndex, "summary_index": 0, "delta": text,
	})
}

func (e *responsesEmitter) closeReasoning() {
	if !e.reasoningOpen {
		return
	}
	text := e.fullThinking.String()
	e.event("response.reasoning_summary_text.done", map[string]interface{}{
		"item_id": e.reasoningItemID, "output_index": e.outputIndex, "summary_index": 0, "text": text,
	})
	e.event("response.reasoning_summary_part.done", map[string]interface{}{
		"item_id": e.reasoningItemID, "output_index": e.outputIndex, "summary_index": 0,
		"part": ResponsesSummaryPart{Type: "summary_text", Text: text},
	})
	item := ResponsesOutputItem{Type: "reasoning", ID: e.reasoningItemID, Status: "completed", Summary: []ResponsesSummaryPart{{Type: "summary_text", Text: text}}}
	e.event("response.output_item.done", map[string]interface{}{"output_index": e.outputIndex, "item": item})
	e.items = append(e.items, item)
	e.outputIndex++
	e.reasoningOpen = false
}

// ── message block ───────────────────────────────────────────────────────────

func (e *responsesEmitter) openMessage() {
	if e.msgOpen {
		return
	}
	e.msgItemID = generateCallId("msg_")
	e.event("response.output_item.added", map[string]interface{}{
		"output_index": e.outputIndex,
		"item":         ResponsesOutputItem{Type: "message", ID: e.msgItemID, Status: "in_progress", Role: "assistant", Content: []ResponsesContentPart{}},
	})
	e.event("response.content_part.added", map[string]interface{}{
		"item_id": e.msgItemID, "output_index": e.outputIndex, "content_index": 0,
		"part": ResponsesContentPart{Type: "output_text", Text: "", Annotations: []interface{}{}},
	})
	e.msgOpen = true
}

func (e *responsesEmitter) textDelta(text string) {
	e.openMessage()
	e.msgText.WriteString(text)
	e.event("response.output_text.delta", map[string]interface{}{
		"item_id": e.msgItemID, "output_index": e.outputIndex, "content_index": 0, "delta": text,
	})
}

func (e *responsesEmitter) closeMessage() {
	if !e.msgOpen {
		return
	}
	text := e.msgText.String()
	e.event("response.output_text.done", map[string]interface{}{
		"item_id": e.msgItemID, "output_index": e.outputIndex, "content_index": 0, "text": text,
	})
	e.event("response.content_part.done", map[string]interface{}{
		"item_id": e.msgItemID, "output_index": e.outputIndex, "content_index": 0,
		"part": ResponsesContentPart{Type: "output_text", Text: text, Annotations: []interface{}{}},
	})
	item := ResponsesOutputItem{
		Type: "message", ID: e.msgItemID, Status: "completed", Role: "assistant",
		Content: []ResponsesContentPart{{Type: "output_text", Text: text, Annotations: []interface{}{}}},
	}
	e.event("response.output_item.done", map[string]interface{}{"output_index": e.outputIndex, "item": item})
	e.items = append(e.items, item)
	e.outputIndex++
	e.msgOpen = false
}

// ── tool-call block ─────────────────────────────────────────────────────────

func (e *responsesEmitter) emitToolCall(tc ToolCall) {
	itemID := generateCallId("fc_")
	e.event("response.output_item.added", map[string]interface{}{
		"output_index": e.outputIndex,
		"item":         ResponsesOutputItem{Type: "function_call", ID: itemID, Status: "in_progress", CallID: tc.ID, Name: tc.Function.Name, Arguments: ""},
	})
	e.event("response.function_call_arguments.delta", map[string]interface{}{
		"item_id": itemID, "output_index": e.outputIndex, "delta": tc.Function.Arguments,
	})
	e.event("response.function_call_arguments.done", map[string]interface{}{
		"item_id": itemID, "output_index": e.outputIndex, "arguments": tc.Function.Arguments,
	})
	item := ResponsesOutputItem{Type: "function_call", ID: itemID, Status: "completed", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
	e.event("response.output_item.done", map[string]interface{}{"output_index": e.outputIndex, "item": item})
	e.items = append(e.items, item)
	e.outputIndex++
}

// ── driver hooks ────────────────────────────────────────────────────────────

func (e *responsesEmitter) processThinking(thinking string) {
	if thinking != "" {
		e.reasoningDelta(thinking)
	}
}

// processContent feeds one content fragment through the classifier and emits
// the resulting text deltas / tool-call blocks. Returns the resolved tool
// payload (non-nil exactly once, when tool calls are confirmed).
func (e *responsesEmitter) processContent(classifier *streamClassifier, content string) *ParsedToolOutput {
	e.rawContent.WriteString(content)
	plain, resolved := classifier.feed(content)
	if plain != "" {
		e.closeReasoning()
		e.textDelta(plain)
	}
	if resolved != nil {
		e.closeReasoning()
		e.closeMessage()
		for _, tc := range resolved.ToolCalls {
			e.emitToolCall(tc)
		}
	}
	return resolved
}

func (e *responsesEmitter) usage() *ResponsesUsage {
	out := countTokens(e.fullThinking.String() + e.rawContent.String())
	return &ResponsesUsage{InputTokens: e.inputTokens, OutputTokens: out, TotalTokens: e.inputTokens + out}
}

// finish flushes any held text and emits the terminal response.completed.
func (e *responsesEmitter) finish(classifier *streamClassifier, resolvedTools *ParsedToolOutput) {
	if resolvedTools == nil {
		if rem := classifier.flush(); rem != "" {
			e.closeReasoning()
			e.textDelta(rem)
		}
		e.closeReasoning()
		e.openMessage() // guarantee a message item even when output was empty
		e.closeMessage()
	}
	e.event("response.completed", map[string]interface{}{"response": e.snapshot("completed", e.items, e.usage(), nil)})
}

// fail emits response.failed. Valid only before the terminal event; the HTTP
// status is already 200 so this is the only way to signal a mid-stream error.
func (e *responsesEmitter) fail(msg string) {
	e.event("response.failed", map[string]interface{}{
		"response": e.snapshot("failed", e.items, nil, &ResponsesError{Type: "api_error", Message: msg}),
	})
}

func responsesStreamHeaders(ctx *fasthttp.RequestCtx) {
	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")
	ctx.SetStatusCode(http.StatusOK)
}

// ── CLI streaming handler ───────────────────────────────────────────────────

func handleResponsesStreamCLI(ctx *fasthttp.RequestCtx, req ResponsesRequest, stdout io.ReadCloser, id string, inputTokens int, um *UsageManager, started time.Time) {
	responsesStreamHeaders(ctx)
	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer stdout.Close()
		e := newResponsesEmitter(w, id, req.Model, inputTokens)
		e.start()

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
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
				// Keep draining so the CLI never blocks on a full stdout pipe.
				e.rawContent.WriteString(extractContentText(msg["content"]))
				e.fullThinking.WriteString(extractThinkingText(msg["content"]))
				continue
			}
			e.processThinking(extractThinkingText(msg["content"]))
			content := extractContentText(msg["content"])
			if content == "" {
				continue
			}
			if r := e.processContent(classifier, content); r != nil {
				resolvedTools = r
			}
		}

		e.finish(classifier, resolvedTools)
		finalizeResponsesLog(ctx, e, resolvedTools)

		out := countTokens(e.fullThinking.String() + e.rawContent.String())
		um.Record(req.Model, inputTokens, out, false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{InputTokens: inputTokens, OutputTokens: out, ThinkingTokens: countTokens(e.fullThinking.String())})
	})
}

func finalizeResponsesLog(ctx *fasthttp.RequestCtx, e *responsesEmitter, resolvedTools *ParsedToolOutput) {
	logID, ok := ctx.UserValue("log_id").(string)
	if !ok || logID == "" {
		return
	}
	parsed := resolvedTools
	if parsed == nil {
		parsed = parseToolCallOutput(e.rawContent.String())
	}
	respData := buildResponsesResponse(e.id, e.model, e.rawContent.String(), parsed, e.inputTokens, e.fullThinking.String())
	UpdateRequestLogResponse(logID, respData)
}

// ── CN direct handlers ──────────────────────────────────────────────────────

// HandleResponsesChat is the CN-gateway direct-API entry for the Responses
// protocol. Only called when cfg.Backend == "cn" (caller checks). Returns true
// to signal fallback to CLI mode.
func (c *DirectClient) HandleResponsesChat(ctx *fasthttp.RequestCtx, req ResponsesRequest, um *UsageManager) bool {
	started := time.Now()
	if req.Stream {
		return c.handleResponsesStreamCN(ctx, req, um, started)
	}
	return c.handleResponsesChatCN(ctx, req, um, started)
}

func (c *DirectClient) responsesCNPreamble(req ResponsesRequest) (ChatRequest, int, []byte, string, string, error) {
	cfg := c.Config.Get()
	toolPrompt := ""
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, false)
	}
	chatReq := responsesRequestToChatRequest(req, toolPrompt)
	sys, prompt := extractSystemAndPrompt(chatReq.Messages)
	inputTokens := countTokens(sys + "\n" + prompt)
	bodyJSON, modelKey, source, err := buildCNRequestBody(chatReq, cfg)
	return chatReq, inputTokens, bodyJSON, modelKey, source, err
}

func (c *DirectClient) handleResponsesChatCN(ctx *fasthttp.RequestCtx, req ResponsesRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()
	if !c.rateLimitOK(ctx, cfg.Token) {
		return false
	}
	_, inputTokens, bodyJSON, modelKey, source, err := c.responsesCNPreamble(req)
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to build CN request body: %v", err), http.StatusInternalServerError)
		return false
	}

	hResp, err := c.sendCNRequest(cfg, bodyJSON, modelKey, source)
	if err != nil {
		AddSystemLog(fmt.Sprintf("CN gateway request failed: %v", err), "error", "direct")
		return true
	}
	defer hResp.Body.Close()

	if hResp.StatusCode == 401 || hResp.StatusCode == 403 {
		AddSystemLog(fmt.Sprintf("CN gateway still returned %d after resign retry, falling back to CLI", hResp.StatusCode), "warn", "direct")
		return true
	}
	if hResp.StatusCode >= 400 {
		b, _ := io.ReadAll(hResp.Body)
		AddSystemLog(fmt.Sprintf("CN gateway returned %d: %s", hResp.StatusCode, string(b)), "warn", "direct")
		return true
	}

	scanner := bufio.NewScanner(hResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	var contentBuilder, thinkingBuilder strings.Builder
	var rawLines []string
	acc := newNativeToolCallAccumulator()
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			rawLines = append(rawLines, line)
		}
		data, ok := cutSSEData(line)
		if !ok {
			continue
		}
		chunkJSON, done, errMsg, errStatus := parseCNSSELine(data)
		if done {
			break
		}
		if errMsg != "" {
			AddSystemLog(fmt.Sprintf("CN gateway SSE error (status %d): %s", errStatus, errMsg), "error", "direct")
			ctx.Error(fmt.Sprintf("CN gateway error: %s", errMsg), http.StatusBadGateway)
			return false
		}
		if chunkJSON == nil {
			continue
		}
		var chunk ChatChunk
		if err := json.Unmarshal(chunkJSON, &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			contentBuilder.WriteString(choice.Content())
			thinkingBuilder.WriteString(choice.ReasoningContent())
			acc.feed(choice)
		}
	}

	finalContent := contentBuilder.String()
	finalThinking := thinkingBuilder.String()
	if finalContent == "" {
		AddSystemLog("Falling back to CLI mode due to empty CN gateway response", "warn", "direct")
		return true
	}

	id := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	parsed := parseToolCallOutput(finalContent)
	if acc.resolved() {
		if r := acc.result(); r != nil {
			parsed = r
		}
	}

	respData := buildResponsesResponse(id, req.Model, finalContent, parsed, inputTokens, finalThinking)

	ctx.SetStatusCode(http.StatusOK)
	ctx.SetContentType("application/json")
	json.NewEncoder(ctx).Encode(respData)
	ctx.SetUserValue("response_body", respData)

	out := countTokens(finalThinking + finalContent)
	um.Record(req.Model, inputTokens, out, false, time.Since(started).Milliseconds())
	ctx.SetUserValue("log_metrics", &LogMetrics{InputTokens: inputTokens, OutputTokens: out, ThinkingTokens: countTokens(finalThinking)})
	if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
		UpdateRequestLogRawLines(logID, rawLines)
	}
	return false
}

func (c *DirectClient) handleResponsesStreamCN(ctx *fasthttp.RequestCtx, req ResponsesRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()
	_, inputTokens, bodyJSON, modelKey, source, err := c.responsesCNPreamble(req)
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to build CN request body: %v", err), http.StatusInternalServerError)
		return false
	}

	hResp, err := c.sendCNRequest(cfg, bodyJSON, modelKey, source)
	if err != nil {
		AddSystemLog(fmt.Sprintf("CN gateway stream request failed: %v", err), "error", "direct")
		return true
	}
	if hResp.StatusCode == 401 || hResp.StatusCode == 403 {
		hResp.Body.Close()
		AddSystemLog(fmt.Sprintf("CN gateway stream still returned %d after resign retry, falling back to CLI", hResp.StatusCode), "warn", "direct")
		return true
	}
	if hResp.StatusCode >= 400 {
		b, _ := io.ReadAll(hResp.Body)
		hResp.Body.Close()
		AddSystemLog(fmt.Sprintf("CN gateway stream returned %d: %s", hResp.StatusCode, string(b)), "warn", "direct")
		return true
	}

	responsesStreamHeaders(ctx)
	id := fmt.Sprintf("resp_%d", time.Now().UnixNano())

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer hResp.Body.Close()
		e := newResponsesEmitter(w, id, req.Model, inputTokens)
		e.start()

		scanner := bufio.NewScanner(hResp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
		classifier := &streamClassifier{}
		acc := newNativeToolCallAccumulator()
		var resolvedTools *ParsedToolOutput
		var rawLines []string

		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				rawLines = append(rawLines, line)
			}
			data, ok := cutSSEData(line)
			if !ok {
				continue
			}
			chunkJSON, done, errMsg, errStatus := parseCNSSELine(data)
			if done {
				AddSystemLog(fmt.Sprintf("[resp-stream] SSE done signal, lines=%d resolvedTools=%v", len(rawLines), resolvedTools != nil), "info", "direct")
				break
			}
			if errMsg != "" {
				AddSystemLog(fmt.Sprintf("[resp-stream] SSE error (status %d): %s", errStatus, errMsg), "error", "direct")
				e.fail(errMsg)
				return
			}
			if chunkJSON == nil {
				continue
			}
			var chunk ChatChunk
			if err := json.Unmarshal(chunkJSON, &chunk); err != nil {
				continue
			}
			for _, choice := range chunk.Choices {
				if resolvedTools != nil {
					e.rawContent.WriteString(choice.Content())
					e.fullThinking.WriteString(choice.ReasoningContent())
					continue
				}
				// Native delta.tool_calls arrive fragmented and are terminated
				// by a separate finish_reason chunk carrying no tool_calls at
				// all, so the accumulator must see every choice.
				acc.feed(choice)
				if acc.resolved() {
					if r := acc.result(); r != nil {
						resolvedTools = r
						AddSystemLog(fmt.Sprintf("[resp-stream] native tool_calls resolved, count=%d", len(r.ToolCalls)), "info", "direct")
						e.emitResolvedToolCalls(resolvedTools)
					}
					continue
				}
				if len(choice.Delta.ToolCalls) > 0 {
					continue
				}
				e.processThinking(choice.ReasoningContent())
				content := choice.Content()
				if content == "" {
					continue
				}
				if r := e.processContent(classifier, content); r != nil {
					resolvedTools = r
				}
			}
		}

		if err := scanner.Err(); err != nil {
			AddSystemLog(fmt.Sprintf("[resp-stream] scanner error: %v, lines=%d", err, len(rawLines)), "error", "direct")
		}
		AddSystemLog(fmt.Sprintf("[resp-stream] scanner EOF, lines=%d resolvedTools=%v", len(rawLines), resolvedTools != nil), "info", "direct")
		e.finish(classifier, resolvedTools)
		finalizeResponsesLog(ctx, e, resolvedTools)
		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			UpdateRequestLogRawLines(logID, rawLines)
		}

		out := countTokens(e.fullThinking.String() + e.rawContent.String())
		um.Record(req.Model, inputTokens, out, false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{InputTokens: inputTokens, OutputTokens: out, ThinkingTokens: countTokens(e.fullThinking.String())})
	})
	return false
}
