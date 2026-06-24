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
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

type DirectClient struct {
	Config *ConfigManager
}

func NewDirectClient(cm *ConfigManager) *DirectClient {
	return &DirectClient{Config: cm}
}

var tokenExchangeCache = make(map[string]string)
var tokenExchangeMutex sync.RWMutex

func exchangeTokenIfNeeded(token, backend string) string {
	if !strings.HasPrefix(token, "pt-") && !strings.HasPrefix(token, "qodercn-") {
		return token
	}

	tokenExchangeMutex.RLock()
	if cached, ok := tokenExchangeCache[token]; ok {
		tokenExchangeMutex.RUnlock()
		return cached
	}
	tokenExchangeMutex.RUnlock()

	baseURL := "https://openapi.qoder.sh/api/v1"
	if strings.ToLower(backend) == "cn" {
		baseURL = "https://openapi.qoder.com.cn/api/v1"
	}

	exchangeURL := baseURL + "/jobToken/exchange"
	reqBody := fmt.Sprintf(`{"personal_token":"%s"}`, token)
	
	req, err := http.NewRequest("POST", exchangeURL, strings.NewReader(reqBody))
	if err != nil {
		return token
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "qoder/1.0.22")
	req.Header.Set("Cosy-Version", "1.0.22")
	req.Header.Set("Cosy-ClientType", "5")
	req.Header.Set("Cosy-MachineOS", "x86_64_win32")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return token
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var result map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
			var actual string
			if t, ok := result["token"].(string); ok && t != "" {
				actual = t
			} else if t, ok := result["device_token"].(string); ok && t != "" {
				actual = t
			} else if t, ok := result["access_token"].(string); ok && t != "" {
				actual = t
			}
			
			if actual != "" {
				AddSystemLog("Successfully exchanged personal token for device token", "info", "direct")
				tokenExchangeMutex.Lock()
				tokenExchangeCache[token] = actual
				tokenExchangeMutex.Unlock()
				return actual
			}
		}
	}
	
	return token
}

func (c *DirectClient) HandleChat(ctx *fasthttp.RequestCtx, req ChatRequest, um *UsageManager) bool {
	started := time.Now()
	cfg := c.Config.Get()

	baseURL := "https://openapi.qoder.sh/api/v1"
	if strings.ToLower(cfg.Backend) == "cn" {
		baseURL = "https://openapi.qoder.com.cn/api/v1"
	}

	reqURL := baseURL + "/chat/completions"

	body, _ := json.Marshal(req)

	if req.Stream {
		return c.handleStream(ctx, reqURL, body, req, um, started)
	}

	hReq, err := http.NewRequest("POST", reqURL, bytes.NewReader(body))
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to create request: %v", err), http.StatusInternalServerError)
		return false
	}

	actualToken := exchangeTokenIfNeeded(cfg.Token, cfg.Backend)
	hReq.Header.Set("Content-Type", "application/json")
	hReq.Header.Set("Authorization", "Bearer "+actualToken)
	// Simulate Qoder CLI / IDE Extension to allow Personal Access Tokens (PAT)
	hReq.Header.Set("User-Agent", "qoder/1.0.22")
	hReq.Header.Set("Cosy-Version", "1.0.22")
	hReq.Header.Set("Cosy-ClientType", "5")
	hReq.Header.Set("Cosy-MachineOS", "x86_64_win32")

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
				InsecureSkipVerify: true,
			},
		},
	}

	hResp, err := client.Do(hReq)
	if err != nil {
		AddSystemLog(fmt.Sprintf("Direct API call failed: %v", err), "error", "direct")
		ctx.Error(fmt.Sprintf("Direct API call failed: %v", err), http.StatusInternalServerError)
		return false
	}
	defer hResp.Body.Close()

	if hResp.StatusCode == 401 || hResp.StatusCode == 404 {
		AddSystemLog(fmt.Sprintf("Direct API backend returned %d, triggering fallback to CLI mode", hResp.StatusCode), "warn", "direct")
		return true
	}

	respBody, _ := io.ReadAll(hResp.Body)

	if hResp.StatusCode >= 400 {
		AddSystemLog(fmt.Sprintf("Direct API backend returned %d: %s", hResp.StatusCode, string(respBody)), "warn", "direct")
	}

	ctx.SetStatusCode(hResp.StatusCode)
	ctx.SetContentType(hResp.Header.Get("Content-Type"))
	ctx.SetBody(respBody)

	sys, prompt := extractSystemAndPrompt(req.Messages)
	um.Record(req.Model, len(sys)+len(prompt), len(respBody), false, time.Since(started).Milliseconds())
	return false
}

func (c *DirectClient) handleStream(ctx *fasthttp.RequestCtx, reqURL string, body []byte, req ChatRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()

	hReq, err := http.NewRequest("POST", reqURL, bytes.NewReader(body))
	if err != nil {
		ctx.Error(fmt.Sprintf("Failed to create stream request: %v", err), http.StatusInternalServerError)
		return false
	}

	actualToken := exchangeTokenIfNeeded(cfg.Token, cfg.Backend)
	hReq.Header.Set("Content-Type", "application/json")
	hReq.Header.Set("Authorization", "Bearer "+actualToken)
	hReq.Header.Set("Accept", "text/event-stream")
	// Simulate Qoder CLI / IDE Extension to allow Personal Access Tokens (PAT)
	hReq.Header.Set("User-Agent", "qoder/1.0.22")
	hReq.Header.Set("Cosy-Version", "1.0.22")
	hReq.Header.Set("Cosy-ClientType", "5")
	hReq.Header.Set("Cosy-MachineOS", "x86_64_win32")
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
				InsecureSkipVerify: true,
			},
		},
	}

	hResp, err := client.Do(hReq)
	if err != nil {
		AddSystemLog(fmt.Sprintf("Failed to create stream request: %v", err), "error", "direct")
		ctx.Error("Failed to create stream request", http.StatusInternalServerError)
		return false
	}

	if hResp.StatusCode == 401 || hResp.StatusCode == 404 {
		hResp.Body.Close()
		AddSystemLog(fmt.Sprintf("Direct Stream backend returned %d, triggering fallback to CLI mode", hResp.StatusCode), "warn", "direct")
		return true
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

	return false
}
