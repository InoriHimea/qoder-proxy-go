package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
		baseURL = "https://openapi.qoder.com.cn/api/v1"
	}

	reqURL := baseURL + "/chat/completions"

	body, _ := json.Marshal(req)

	if req.Stream {
		c.handleStream(ctx, reqURL, body, req, um, started)
		return
	}

	hReq, err := http.NewRequest("POST", reqURL, bytes.NewReader(body))
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return
	}

	hReq.Header.Set("Content-Type", "application/json")
	hReq.Header.Set("Authorization", "Bearer "+cfg.Token)

	proxyFunc := http.ProxyFromEnvironment
	if cfg.ProxyURL != "" {
		if pURL, err := url.Parse(cfg.ProxyURL); err == nil {
			proxyFunc = http.ProxyURL(pURL)
		} else {
			AddSystemLog(fmt.Sprintf("Invalid custom ProxyURL '%s': %v. Falling back to env.", cfg.ProxyURL, err), "warn", "direct")
		}
	}

	client := &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			Proxy:               proxyFunc,
			TLSHandshakeTimeout: 30 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
		},
	}

	hResp, err := client.Do(hReq)
	if err != nil {
		AddSystemLog(fmt.Sprintf("Direct API call failed: %v", err), "error", "direct")
		ctx.Error(fmt.Sprintf("Direct API call failed: %v", err), http.StatusInternalServerError)
		return
	}
	defer hResp.Body.Close()

	respBody, _ := io.ReadAll(hResp.Body)

	if hResp.StatusCode >= 400 {
		AddSystemLog(fmt.Sprintf("Direct API backend returned %d: %s", hResp.StatusCode, string(respBody)), "warn", "direct")
	}

	ctx.SetStatusCode(hResp.StatusCode)
	ctx.SetContentType(hResp.Header.Get("Content-Type"))
	ctx.SetBody(respBody)

	sys, prompt := extractSystemAndPrompt(req.Messages)
	um.Record(req.Model, len(sys)+len(prompt), len(respBody), false, time.Since(started).Milliseconds())
}

func (c *DirectClient) handleStream(ctx *fasthttp.RequestCtx, reqURL string, body []byte, req ChatRequest, um *UsageManager, started time.Time) {
	cfg := c.Config.Get()

	hReq, err := http.NewRequest("POST", reqURL, bytes.NewReader(body))
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to create stream request: %v", err), http.StatusInternalServerError)
		return
	}

	hReq.Header.Set("Content-Type", "application/json")
	hReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	hReq.Header.Set("Accept", "text/event-stream")
	hReq.Header.Set("Cache-Control", "no-cache")
	hReq.Header.Set("Connection", "keep-alive")

	proxyFunc := http.ProxyFromEnvironment
	if cfg.ProxyURL != "" {
		if pURL, err := url.Parse(cfg.ProxyURL); err == nil {
			proxyFunc = http.ProxyURL(pURL)
		} else {
			AddSystemLog(fmt.Sprintf("Invalid custom ProxyURL '%s': %v. Falling back to env.", cfg.ProxyURL, err), "warn", "direct")
		}
	}

	client := &http.Client{
		Timeout: 15 * time.Minute,
		Transport: &http.Transport{
			Proxy:               proxyFunc,
			TLSHandshakeTimeout: 30 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
		},
	}

	hResp, err := client.Do(hReq)
	if err != nil {
		AddSystemLog(fmt.Sprintf("Direct Stream call failed: %v", err), "error", "direct")
		ctx.Error(fmt.Sprintf("Direct Stream call failed: %v", err), http.StatusInternalServerError)
		return
	}

	if hResp.StatusCode >= 400 {
		AddSystemLog(fmt.Sprintf("Direct Stream backend returned %d", hResp.StatusCode), "warn", "direct")
	}
	// We don't defer hResp.Body.Close() here because we pass it to the stream writer

	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")
	ctx.SetStatusCode(hResp.StatusCode)

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer hResp.Body.Close()

		var fullContent strings.Builder

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

				// Try to extract content chunks for logging
				lines := strings.Split(string(buf[:n]), "\n")
				for _, line := range lines {
					if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
						var chunk struct {
							Choices []struct {
								Delta struct {
									Content string `json:"content"`
								} `json:"delta"`
							} `json:"choices"`
						}
						if err := json.Unmarshal([]byte(line[6:]), &chunk); err == nil {
							if len(chunk.Choices) > 0 {
								fullContent.WriteString(chunk.Choices[0].Delta.Content)
							}
						}
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					AddSystemLog(fmt.Sprintf("Direct Stream read error: %v", err), "error", "direct")
				}
				break
			}
		}

		// If we are tracking request logs for this request, update it with the full content
		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			UpdateRequestLogResponse(logID, map[string]interface{}{"streamed_content": fullContent.String()})
		}

		sys, prompt := extractSystemAndPrompt(req.Messages)
		um.Record(req.Model, len(sys)+len(prompt), outLen/50, false, time.Since(started).Milliseconds())
	})
}
