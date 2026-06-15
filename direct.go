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
	hReq.Header.Set("Accept", "text/event-stream")
	hReq.SetBody(body)
	
	if req.Stream {
		c.handleStream(ctx, hReq, req, um, started)
		return
	}

	hResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(hResp)

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
	// For streaming, we use a client that won't buffer the response
	client := &fasthttp.Client{
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
	}

	hResp := fasthttp.AcquireResponse()
	// No defer ReleaseResponse here because we'll handle it or it's piped
	
	err := client.DoStream(hReq, hResp)
	if err != nil {
		ctx.Error(fmt.Sprintf("Direct Stream failed to initiate: %v", err), http.StatusInternalServerError)
		fasthttp.ReleaseResponse(hResp)
		return
	}

	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")
	ctx.SetStatusCode(hResp.StatusCode())

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer fasthttp.ReleaseResponse(hResp)
		reader := hResp.BodyStream()
		if reader == nil {
			return
		}
		
		// Copy chunks from upstream to client
		// Using a small buffer for minimal latency
		buf := make([]byte, 4096)
		outLen := 0
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				outLen += n
				w.Write(buf[:n])
				w.Flush()
			}
			if err != nil {
				break
			}
		}
		um.Record(req.Model, len(messagesToPrompt(req.Messages)), outLen/50, false, time.Since(started).Milliseconds())
	})
}
