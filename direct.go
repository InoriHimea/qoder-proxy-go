package main

import (
	"bufio"
	"bytes"
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
	
	if req.Stream {
		c.handleStream(ctx, url, body, req, um, started)
		return
	}

	hReq := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(hReq)
	
	hReq.SetRequestURI(url)
	hReq.Header.SetMethod("POST")
	hReq.Header.SetContentType("application/json")
	hReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	hReq.SetBody(body)
	
	hResp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(hResp)

	err := fasthttp.Do(hReq, hResp)
	if err != nil {
		ctx.Error(fmt.Sprintf("Direct API call failed: %v", err), http.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(hResp.StatusCode())
	ctx.SetBody(hResp.Body())
	
	um.Record(req.Model, len(messagesToPrompt(req.Messages)), len(hResp.Body()), false, time.Since(started).Milliseconds())
}

func (c *DirectClient) handleStream(ctx *fasthttp.RequestCtx, url string, body []byte, req ChatRequest, um *UsageManager, started time.Time) {
	cfg := c.Config.Get()
	
	hReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to create stream request: %v", err), http.StatusInternalServerError)
		return
	}
	
	hReq.Header.Set("Content-Type", "application/json")
	hReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	hReq.Header.Set("Accept", "text/event-stream")
	hReq.Header.Set("Cache-Control", "no-cache")
	hReq.Header.Set("Connection", "keep-alive")

	client := &http.Client{
		Timeout: 15 * time.Minute,
	}

	hResp, err := client.Do(hReq)
	if err != nil {
		ctx.Error(fmt.Sprintf("Direct Stream call failed: %v", err), http.StatusInternalServerError)
		return
	}
	// We don't defer hResp.Body.Close() here because we pass it to the stream writer

	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")
	ctx.SetStatusCode(hResp.StatusCode)

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer hResp.Body.Close()
		
		// Use a buffer to read from the stream and write to the client
		buf := make([]byte, 4096)
		outLen := 0
		for {
			n, err := hResp.Body.Read(buf)
			if n > 0 {
				outLen += n
				_, writeErr := w.Write(buf[:n])
				if writeErr != nil {
					break // Client disconnected
				}
				w.Flush()
			}
			if err != nil {
				if err != io.EOF {
					AddSystemLog(fmt.Sprintf("Direct Stream read error: %v", err), "error", "direct")
				}
				break
			}
		}
		um.Record(req.Model, len(messagesToPrompt(req.Messages)), outLen/50, false, time.Since(started).Milliseconds())
	})
}
