package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

type DirectClient struct {
	Config *ConfigManager
}

func NewDirectClient(cm *ConfigManager) *DirectClient {
	return &DirectClient{Config: cm}
}

func (c *DirectClient) HandleChat(ctx *fasthttp.RequestCtx, req ChatRequest, um *UsageManager) {
	started := time.Now()
	cfg := c.Config.Get()
	
	baseURL := "https://openapi.qoder.sh/api/v1"
	if strings.ToLower(cfg.Backend) == "cn" {
		baseURL = "https://openapi.qoderwork.cn/api/v1"
	}

	url := baseURL + "/chat/completions"
	
	body, _ := json.Marshal(req)
	
	hReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(hReq)
	
	hReq.SetRequestURI(url)
	hReq.Header.SetMethod("POST")
	hReq.Header.SetContentType("application/json")
	hReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	hReq.SetBody(body)
	
	hResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(hResp)
	
	if req.Stream {
		c.handleStream(ctx, hReq, req, um, started)
		return
	}

	err := fasthttp.Do(hReq, hResp)
	if err != nil {
		ctx.Error(fmt.Sprintf("Direct API call failed: %v", err), http.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(hResp.StatusCode())
	ctx.SetBody(hResp.Body())
	
	// Record usage (rough estimate)
	um.Record(req.Model, len(messagesToPrompt(req.Messages)), len(hResp.Body()), false, time.Since(started).Milliseconds())
}

func (c *DirectClient) handleStream(ctx *fasthttp.RequestCtx, hReq *fasthttp.Request, req ChatRequest, um *UsageManager, started time.Time) {
	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")

	err := fasthttp.Do(hReq, &ctx.Response)
	if err != nil {
		AddSystemLog(fmt.Sprintf("Direct Stream failed: %v", err), "error", "direct")
	}
	
	// Note: fasthttp.Do with ctx.Response will handle the streaming if the backend sends it correctly.
	// But we might want to intercept it to record usage.
}
