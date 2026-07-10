package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/InoriHimea/qoder-proxy-go/wasmsigner"
	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
)

// cnGatewayHost is the bare-host endpoint the CN legacy transport signs
// requests against; the WASM signer appends the inference path itself.
const cnGatewayHost = "https://gateway.qoder.com.cn"

type DirectClient struct {
	Config *ConfigManager

	signerMu sync.Mutex
	signer   *wasmsigner.Signer
}

func NewDirectClient(cm *ConfigManager) *DirectClient {
	return &DirectClient{Config: cm}
}

// rawJSONResponseBody wraps a raw HTTP response body so it round-trips
// through response_body logging (json.Marshal) as the JSON it already is,
// instead of being base64-encoded (which is what json.Marshal does to a
// bare []byte). Falls back to the raw string when the body isn't valid JSON.
func rawJSONResponseBody(b []byte) interface{} {
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
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

// refreshDeviceToken attempts to refresh an expired dt- token via Qoder's
// deviceToken/refresh endpoint.  Returns the original token on failure.
func refreshDeviceToken(currentDT, backend string) string {
	if !strings.HasPrefix(currentDT, "dt-") {
		return currentDT
	}

	tokenExchangeMutex.RLock()
	if cached, ok := tokenExchangeCache[currentDT]; ok {
		tokenExchangeMutex.RUnlock()
		return cached
	}
	tokenExchangeMutex.RUnlock()

	baseURL := "https://openapi.qoder.sh/api/v1"
	if strings.ToLower(backend) == "cn" {
		baseURL = "https://openapi.qoder.com.cn/api/v1"
	}

	refreshURL := baseURL + "/deviceToken/refresh"
	reqBody := fmt.Sprintf(`{"device_token":"%s"}`, currentDT)

	req, err := http.NewRequest("POST", refreshURL, strings.NewReader(reqBody))
	if err != nil {
		return currentDT
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "qoder/1.0.22")
	req.Header.Set("Cosy-Version", "1.0.22")
	req.Header.Set("Cosy-ClientType", "5")
	req.Header.Set("Cosy-MachineOS", "x86_64_win32")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return currentDT
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var result map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil {
			var newToken string
			if t, ok := result["token"].(string); ok && t != "" {
				newToken = t
			} else if t, ok := result["device_token"].(string); ok && t != "" {
				newToken = t
			} else if t, ok := result["access_token"].(string); ok && t != "" {
				newToken = t
			}

			if newToken != "" && newToken != currentDT {
				AddSystemLog("Successfully refreshed device token", "info", "direct")
				tokenExchangeMutex.Lock()
				tokenExchangeCache[currentDT] = newToken
				tokenExchangeMutex.Unlock()
				return newToken
			}
		}
	}

	AddSystemLog(fmt.Sprintf("Device token refresh failed (status %d). Token may need manual renewal.", resp.StatusCode), "warn", "direct")
	return currentDT
}

// refreshDeviceTokenViaOAuth attempts to refresh an expired dt- token via
// the OAuth refresh_token grant. Falls back to the legacy refresh endpoint
// when no refresh_token is available.
func (c *DirectClient) refreshDeviceTokenViaOAuth(currentDT, backend, refreshToken string) string {
	if !strings.HasPrefix(currentDT, "dt-") || refreshToken == "" {
		return refreshDeviceToken(currentDT, backend)
	}

	// Check cache first
	tokenExchangeMutex.RLock()
	if cached, ok := tokenExchangeCache[currentDT]; ok {
		tokenExchangeMutex.RUnlock()
		return cached
	}
	tokenExchangeMutex.RUnlock()

	oauthClient := NewOAuthClient(backend)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := oauthClient.RefreshToken(ctx, refreshToken)
	if err != nil {
		AddSystemLog(fmt.Sprintf("OAuth token refresh via refresh_token failed: %v, falling back to legacy refresh", err), "warn", "direct")
		return refreshDeviceToken(currentDT, backend)
	}

	newToken := result.Token
	if newToken == "" || newToken == currentDT {
		AddSystemLog("OAuth refresh returned empty or identical token, falling back to legacy refresh", "warn", "direct")
		return refreshDeviceToken(currentDT, backend)
	}

	// Update config with new token and refresh_token
	cfg := c.Config.Get()
	cfg.Token = newToken
	if result.RefreshToken != "" {
		cfg.RefreshToken = result.RefreshToken
	}
	if result.UserID != "" {
		cfg.UserID = result.UserID
	}
	if result.ExpireTime != 0 {
		cfg.ExpireTime = result.ExpireTime
	}
	if err := c.Config.Update(cfg); err != nil {
		AddSystemLog(fmt.Sprintf("Failed to persist refreshed token to config: %v", err), "error", "direct")
	} else {
		AddSystemLog("OAuth token refreshed successfully via refresh_token, new token stored", "info", "direct")
		tokenExchangeMutex.Lock()
		tokenExchangeCache[currentDT] = newToken
		tokenExchangeMutex.Unlock()
	}

	return newToken
}

// newDirectHTTPClient builds the shared http.Client used for both the
// non-CN bearer-token transport and the CN signed-gateway transport.
func newDirectHTTPClient(cfg Config) *http.Client {
	proxyFunc := http.ProxyFromEnvironment
	if cfg.ProxyURL != "" {
		if pURL, err := url.Parse(cfg.ProxyURL); err == nil {
			proxyFunc = http.ProxyURL(pURL)
		} else {
			AddSystemLog(fmt.Sprintf("Invalid custom ProxyURL '%s': %v. Falling back to env.", cfg.ProxyURL, err), "warn", "direct")
		}
	}

	return &http.Client{
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
}

func (c *DirectClient) HandleChat(ctx *fasthttp.RequestCtx, req ChatRequest, um *UsageManager) bool {
	started := time.Now()
	cfg := c.Config.Get()

	if strings.ToLower(cfg.Backend) == "cn" {
		if req.Stream {
			return c.handleStreamCN(ctx, req, um, started)
		}
		return c.handleChatCN(ctx, req, um, started)
	}

	baseURL := "https://api2-v2.qoder.sh"
	reqURL := baseURL + "/model/v1/chat/completions"

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

	client := newDirectHTTPClient(cfg)

	hResp, err := client.Do(hReq)
	if err != nil {
		AddSystemLog(fmt.Sprintf("Direct API call failed: %v", err), "error", "direct")
		ctx.Error(fmt.Sprintf("Direct API call failed: %v", err), http.StatusInternalServerError)
		return false
	}
	defer hResp.Body.Close()

	if hResp.StatusCode == 401 || hResp.StatusCode == 404 {
		// Try refreshing dt- token via OAuth flow before falling back to CLI
		if strings.HasPrefix(actualToken, "dt-") {
			refreshed := c.refreshDeviceTokenViaOAuth(actualToken, cfg.Backend, cfg.RefreshToken)
			if refreshed != actualToken {
				AddSystemLog("Retrying direct API with refreshed OAuth token", "info", "direct")
				hReq2, _ := http.NewRequest("POST", reqURL, bytes.NewReader(body))
				hReq2.Header = hReq.Header.Clone()
				hReq2.Header.Set("Authorization", "Bearer "+refreshed)
				hResp2, err2 := client.Do(hReq2)
				if err2 == nil && hResp2.StatusCode != 401 && hResp2.StatusCode != 404 {
					defer hResp2.Body.Close()
					respBody2, _ := io.ReadAll(hResp2.Body)
					if hResp2.StatusCode < 400 {
						ctx.SetStatusCode(hResp2.StatusCode)
						ctx.SetContentType(hResp2.Header.Get("Content-Type"))
						ctx.SetBody(respBody2)
						ctx.SetUserValue("response_body", rawJSONResponseBody(respBody2))
						sys, prompt := extractSystemAndPrompt(req.Messages)
						um.Record(req.Model, countTokens(sys+"\n"+prompt), countTokens(string(respBody2)), false, time.Since(started).Milliseconds())
						ctx.SetUserValue("log_metrics", &LogMetrics{
							InputTokens:  countTokens(sys + "\n" + prompt),
							OutputTokens: countTokens(string(respBody2)),
						})
						return false
					}
				} else if err2 == nil {
					hResp2.Body.Close()
				}
			}
		}
		AddSystemLog(fmt.Sprintf("Direct API backend returned %d, triggering fallback to CLI mode", hResp.StatusCode), "warn", "direct")
		return true
	}

	respBody, _ := io.ReadAll(hResp.Body)

	if hResp.StatusCode >= 400 {
		AddSystemLog(fmt.Sprintf("Direct API backend returned %d: %s", hResp.StatusCode, string(respBody)), "warn", "direct")
		if hResp.StatusCode != 401 && hResp.StatusCode != 404 {
			ctx.SetStatusCode(hResp.StatusCode)
			ctx.SetContentType("application/json")
			ctx.SetBody(respBody)
			ctx.SetUserValue("response_body", rawJSONResponseBody(respBody))
			return false
		}
		// Retry once with refreshed token on non-auth errors race
		if strings.HasPrefix(actualToken, "dt-") {
			refreshed := refreshDeviceToken(actualToken, cfg.Backend)
			if refreshed != actualToken {
				hReq2, _ := http.NewRequest("POST", reqURL, bytes.NewReader(body))
				hReq2.Header = hReq.Header.Clone()
				hReq2.Header.Set("Authorization", "Bearer "+refreshed)
				if hResp2, err2 := client.Do(hReq2); err2 == nil {
					defer hResp2.Body.Close()
					respBody2, _ := io.ReadAll(hResp2.Body)
					if hResp2.StatusCode < 400 {
						ctx.SetStatusCode(hResp2.StatusCode)
						ctx.SetContentType(hResp2.Header.Get("Content-Type"))
						ctx.SetBody(respBody2)
						ctx.SetUserValue("response_body", rawJSONResponseBody(respBody2))
						sys, prompt := extractSystemAndPrompt(req.Messages)
						um.Record(req.Model, countTokens(sys+"\n"+prompt), countTokens(string(respBody2)), false, time.Since(started).Milliseconds())
						ctx.SetUserValue("log_metrics", &LogMetrics{
							InputTokens:  countTokens(sys + "\n" + prompt),
							OutputTokens: countTokens(string(respBody2)),
						})
						return false
					}
				}
			}
		}
		return true
	}

	sys, prompt := extractSystemAndPrompt(req.Messages)
	um.Record(req.Model, countTokens(sys+"\n"+prompt), countTokens(string(respBody)), false, time.Since(started).Milliseconds())
	ctx.SetUserValue("response_body", rawJSONResponseBody(respBody))
	ctx.SetUserValue("log_metrics", &LogMetrics{
		InputTokens:  countTokens(sys + "\n" + prompt),
		OutputTokens: countTokens(string(respBody)),
	})
	return false
}

func (c *DirectClient) handleStream(ctx *fasthttp.RequestCtx, reqURL string, body []byte, req ChatRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()

	streamURL := reqURL

	hReq, err := http.NewRequest("POST", streamURL, bytes.NewReader(body))
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

	client := newDirectHTTPClient(cfg)

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
		b, _ := io.ReadAll(hResp.Body)
		hResp.Body.Close()
		AddSystemLog(fmt.Sprintf("Direct Stream backend returned %d: %s", hResp.StatusCode, string(b)), "warn", "direct")
		return true
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
		for {
			n, err := hResp.Body.Read(buf)
			if n > 0 {
				_, writeErr := w.Write(buf[:n])
				if writeErr != nil {
					break // Client disconnected
				}
				w.Flush()

				// Try to extract content chunks for logging
				lines := strings.Split(string(buf[:n]), "\n")
				for _, line := range lines {
					data, ok := cutSSEData(line)
					if ok && data != "[DONE]" {
						var chunk struct {
							Choices []struct {
								Delta struct {
									Content string `json:"content"`
								} `json:"delta"`
							} `json:"choices"`
						}
						if err := json.Unmarshal([]byte(data), &chunk); err == nil {
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
		um.Record(req.Model, countTokens(sys+"\n"+prompt), countTokens(fullContent.String()), false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{
			InputTokens:  countTokens(sys + "\n" + prompt),
			OutputTokens: countTokens(fullContent.String()),
		})
	})

	return false
}

// ── CN legacy gateway transport (wasmsigner-signed) ─────────────────────────

// getSigner lazily creates the long-lived WASM signer bound to this
// install's machine ID. The signer's internal QoderContext is rebuilt only
// when the signed-for user identity changes (see wasmsigner.Signer).
func (c *DirectClient) getSigner(machineID string) (*wasmsigner.Signer, error) {
	c.signerMu.Lock()
	defer c.signerMu.Unlock()
	if c.signer != nil {
		return c.signer, nil
	}
	s, err := wasmsigner.New(machineID)
	if err != nil {
		return nil, err
	}
	c.signer = s
	return s, nil
}

// buildCNRequestBody maps an incoming ChatRequest into the snake_case body
// shape the CN legacy gateway expects (mirrors the JS bundle's xdA()).
// Note: the real client always sends stream:true on this transport even
// for "non-streaming" callers — the proxy internally aggregates the SSE
// response when the caller didn't ask for a stream.
func buildCNRequestBody(req ChatRequest, cfg Config) (bodyJSON []byte, modelKey string, source string, err error) {
	requestID := uuid.New().String()
	sessionID := uuid.New().String()

	var sysSb strings.Builder
	var msgs []map[string]interface{}
	lastUserContent := ""
	for _, m := range req.Messages {
		content := extractContentText(m.Content)
		if strings.ToLower(m.Role) == "system" {
			if content != "" {
				sysSb.WriteString(content + "\n\n")
			}
			continue
		}
		msgs = append(msgs, map[string]interface{}{"role": m.Role, "content": content})
		if strings.ToLower(m.Role) == "user" {
			lastUserContent = content
		}
	}
	sysText := strings.TrimSpace(sysSb.String())
	if sysText != "" {
		msgs = append([]map[string]interface{}{{"role": "system", "content": sysText}}, msgs...)
	}

	var modelDesc string
	for _, m := range cfg.Models {
		if m.ID == req.Model {
			modelDesc = m.Description
			break
		}
	}
	isReasoning := req.ReasoningEffort != "" || strings.Contains(strings.ToLower(modelDesc), "reasoning")

	modelConfig := map[string]interface{}{
		"key":              req.Model,
		"display_name":     req.Model,
		"model":            "",
		"format":           "openai",
		"is_vl":            false,
		"is_reasoning":     isReasoning,
		"api_key":          "",
		"url":              "",
		"source":           "system",
		"max_input_tokens": 128000,
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	parameters := map[string]interface{}{"max_tokens": maxTokens}
	if isReasoning && req.ReasoningEffort != "" {
		parameters["reasoning_effort"] = req.ReasoningEffort
	}
	if req.ToolChoice != nil {
		parameters["tool_choice"] = req.ToolChoice
	}

	var tools interface{} = []interface{}{}
	if len(req.Tools) > 0 {
		var t interface{}
		if jsonErr := json.Unmarshal(req.Tools, &t); jsonErr == nil {
			tools = t
		}
	}

	chatContext := map[string]interface{}{
		"text":     lastUserContent,
		"features": []interface{}{},
		"extra": map[string]interface{}{
			"context":         []interface{}{},
			"modelConfig":     map[string]interface{}{"key": req.Model, "is_reasoning": isReasoning},
			"originalContent": lastUserContent,
		},
		"chatPrompt": "",
		"imageUrls":  nil,
	}

	body := map[string]interface{}{
		"request_id":       requestID,
		"request_set_id":   requestID,
		"chat_record_id":   requestID,
		"session_id":       sessionID,
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"chat_context":     chatContext,
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"session_type":     "",
		"aliyun_user_type": "",
		"model_config":     modelConfig,
		"custom_model":     nil,
		"system":           sysText,
		"messages":         msgs,
		"tools":            tools,
		"parameters":       parameters,
	}

	bodyJSON, err = json.Marshal(body)
	return bodyJSON, req.Model, "system", err
}

// signCNRequest signs bodyJSON for the CN gateway using the persisted user
// identity in cfg.
func (c *DirectClient) signCNRequest(cfg Config, bodyJSON []byte, modelKey, source string) (*wasmsigner.SignedRequest, error) {
	signer, err := c.getSigner(cfg.MachineID)
	if err != nil {
		return nil, err
	}
	u := wasmsigner.UserInfo{
		UID:              cfg.UserID,
		OrganizationID:   cfg.OrganizationID,
		OrganizationTags: cfg.OrganizationTags,
		DataPolicyAgreed: cfg.DataPolicyAgreed,
	}
	return signer.SignInferRequest(u, cnGatewayHost, string(bodyJSON), &modelKey, &source)
}

// refreshCNIdentity re-fetches org/user info from the OpenAPI host (if a
// token is available) and invalidates the signer's cached QoderContext so
// the next sign rebuilds it from fresh identity fields.
func (c *DirectClient) refreshCNIdentity(cfg Config) Config {
	if signer, err := c.getSigner(cfg.MachineID); err == nil {
		signer.Invalidate()
	}
	if cfg.Token != "" {
		oc := NewOAuthClient(cfg.Backend)
		ctxTO, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if info, err := oc.GetUserInfo(ctxTO, cfg.Token); err == nil {
			if info.OrganizationID != "" {
				cfg.OrganizationID = info.OrganizationID
			}
			if info.OrganizationTags != nil {
				cfg.OrganizationTags = info.OrganizationTags
			}
			cfg.DataPolicyAgreed = info.DataPolicyAgreed
			c.Config.Update(cfg)
		} else {
			AddSystemLog(fmt.Sprintf("CN identity refresh failed: %v", err), "warn", "direct")
		}
	}
	return cfg
}

// sendCNRequest signs and POSTs bodyJSON to the CN gateway, re-signing and
// retrying once on 401/403 (mirrors QEn()'s force-refresh-then-resign loop).
func (c *DirectClient) sendCNRequest(cfg Config, bodyJSON []byte, modelKey, source string) (*http.Response, error) {
	client := newDirectHTTPClient(cfg)

	doOnce := func(cfg Config) (*http.Response, error) {
		signed, err := c.signCNRequest(cfg, bodyJSON, modelKey, source)
		if err != nil {
			return nil, fmt.Errorf("sign CN request: %w", err)
		}
		hReq, err := http.NewRequest("POST", signed.URL, strings.NewReader(signed.Body))
		if err != nil {
			return nil, err
		}
		for k, v := range signed.Headers {
			hReq.Header.Set(k, v)
		}
		hReq.Header.Set("Accept", "text/event-stream")
		return client.Do(hReq)
	}

	hResp, err := doOnce(cfg)
	if err != nil {
		return nil, err
	}

	if hResp.StatusCode == 401 || hResp.StatusCode == 403 {
		hResp.Body.Close()
		AddSystemLog(fmt.Sprintf("CN gateway returned %d, refreshing identity and re-signing", hResp.StatusCode), "warn", "direct")
		cfg = c.refreshCNIdentity(cfg)
		hResp, err = doOnce(cfg)
		if err != nil {
			return nil, err
		}
	}

	return hResp, nil
}

// cutSSEData strips a "data:" SSE field prefix, tolerating both "data: "
// (per SSE spec, one optional leading space) and "data:" with no space —
// the CN gateway sends the latter, which strings.HasPrefix(line, "data: ")
// silently fails to match, dropping every chunk without error.
func cutSSEData(line string) (data string, ok bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "), true
}

// cnSSEEnvelope matches the error-path SSE payload shape from the JS
// bundle's xO(): {statusCodeValue, statusCode, body}. Success-path chunks
// are NOT enveloped and arrive as plain OpenAI-shape JSON. statusCode is
// observed as both a string ("OK") and a number depending on gateway
// path, so it's left untyped — only StatusCodeValue is actually used.
type cnSSEEnvelope struct {
	StatusCodeValue *float64        `json:"statusCodeValue"`
	StatusCode      json.RawMessage `json:"statusCode"`
	Body            string          `json:"body"`
}

// isCNSSESentinel matches no-op control lines that should be skipped
// without ending or failing the stream.
func isCNSSESentinel(data string) bool {
	return data == "[NOT_EXCEED_QUOTA]" || strings.HasPrefix(data, "[NOTIFICATIONS]")
}

// parseCNSSELine inspects one SSE "data:" payload. If it is a non-200
// error envelope or a quota-exceeded sentinel, ok=false and errMsg/errStatus
// describe the failure. Otherwise chunk is the (already-unwrapped) chunk
// JSON to forward/parse, or nil if the line was a no-op sentinel.
func parseCNSSELine(data string) (chunk []byte, done bool, errMsg string, errStatus int) {
	if data == "[DONE]" {
		return nil, true, "", 0
	}
	if strings.HasPrefix(data, "[EXCEED_QUOTA]") {
		msg := strings.TrimSpace(strings.TrimPrefix(data, "[EXCEED_QUOTA]"))
		if msg == "" {
			msg = "quota exceeded"
		}
		return nil, false, msg, http.StatusPaymentRequired
	}
	if isCNSSESentinel(data) {
		return nil, true, "", 0
	}
	var env cnSSEEnvelope
	if err := json.Unmarshal([]byte(data), &env); err == nil && env.StatusCodeValue != nil {
		if int(*env.StatusCodeValue) != 200 {
			return nil, false, env.Body, int(*env.StatusCodeValue)
		}
		body := strings.TrimSpace(env.Body)
		if body == "" || isCNSSESentinel(body) {
			return nil, false, "", 0
		}
		return []byte(body), false, "", 0
	}
	return []byte(data), false, "", 0
}

func (c *DirectClient) handleChatCN(ctx *fasthttp.RequestCtx, req ChatRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()

	bodyJSON, modelKey, source, err := buildCNRequestBody(req, cfg)
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

	var contentBuilder strings.Builder
	var thinkingBuilder strings.Builder
	var rawLines []string
	model := req.Model
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
		if err := json.Unmarshal(chunkJSON, &chunk); err == nil {
			if chunk.Model != "" {
				model = chunk.Model
			}
			for _, choice := range chunk.Choices {
				contentBuilder.WriteString(choice.Content())
				thinkingBuilder.WriteString(choice.ReasoningContent())
			}
		}
	}

	finalContent := contentBuilder.String()
	finalThinking := thinkingBuilder.String()
	if finalContent == "" {
		dump := strings.Join(rawLines, " | ")
		if len(dump) > 2000 {
			dump = dump[:2000]
		}
		AddSystemLog(fmt.Sprintf("CN gateway produced empty content, raw SSE lines: %s", dump), "warn", "direct")
		AddSystemLog("Falling back to CLI mode due to empty CN gateway response", "warn", "direct")
		return true
	}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	msgMap := map[string]interface{}{"role": "assistant", "content": finalContent}
	if finalThinking != "" {
		msgMap["reasoning_content"] = finalThinking
	}
	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msgMap,
				"finish_reason": "stop",
			},
		},
	}
	respBody, _ := json.Marshal(resp)
	ctx.SetStatusCode(http.StatusOK)
	ctx.SetContentType("application/json")
	ctx.SetBody(respBody)
	ctx.SetUserValue("response_body", resp)

	sys, prompt := extractSystemAndPrompt(req.Messages)
	um.Record(req.Model, countTokens(sys+"\n"+prompt), countTokens(finalThinking+finalContent), false, time.Since(started).Milliseconds())
	ctx.SetUserValue("log_metrics", &LogMetrics{
		InputTokens:    countTokens(sys + "\n" + prompt),
		OutputTokens:   countTokens(finalThinking + finalContent),
		ThinkingTokens: countTokens(finalThinking),
	})

	// Persist raw SSE lines so the dashboard can show the original stream
	if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
		UpdateRequestLogRawLines(logID, rawLines)
	}

	return false
}

func (c *DirectClient) handleStreamCN(ctx *fasthttp.RequestCtx, req ChatRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()

	bodyJSON, modelKey, source, err := buildCNRequestBody(req, cfg)
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

	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")
	ctx.SetStatusCode(http.StatusOK)

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer hResp.Body.Close()

		scanner := bufio.NewScanner(hResp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

		var fullContent strings.Builder
		var fullThinking strings.Builder
		var rawLines []string
		streamErr := false
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
				fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
					"error": map[string]interface{}{"message": errMsg, "code": errStatus},
				}))
				w.Flush()
				streamErr = true
				break
			}
			if chunkJSON == nil {
				continue
			}
			var chunk ChatChunk
			forwardJSON := chunkJSON
			if err := json.Unmarshal(chunkJSON, &chunk); err == nil {
				normalized := false
				for i, choice := range chunk.Choices {
					fullContent.WriteString(choice.Content())
					fullThinking.WriteString(choice.ReasoningContent())
					if choice.Delta.Content == "" && choice.Message.Content != "" {
						chunk.Choices[i].Delta.Content = choice.Message.Content
						normalized = true
					}
					if choice.Delta.ReasoningContent == "" && choice.Message.ReasoningContent != "" {
						chunk.Choices[i].Delta.ReasoningContent = choice.Message.ReasoningContent
						normalized = true
					}
				}
				if normalized {
					if b, merr := json.Marshal(chunk); merr == nil {
						forwardJSON = b
					}
				}
			}
			if _, werr := w.Write([]byte("data: ")); werr != nil {
				return
			}
			if _, werr := w.Write(forwardJSON); werr != nil {
				return
			}
			if _, werr := w.Write([]byte("\n\n")); werr != nil {
				return
			}
			w.Flush()
		}

		if !streamErr {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
		}

		if !streamErr && fullContent.Len() == 0 {
			dump := strings.Join(rawLines, " | ")
			if len(dump) > 2000 {
				dump = dump[:2000]
			}
			AddSystemLog(fmt.Sprintf("CN gateway stream produced empty content, raw SSE lines: %s", dump), "warn", "direct")
			// Can't return true from inside SetBodyStreamWriter (SSE headers already sent),
			// so send an error event with a special code so the client knows to retry.
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]interface{}{
				"error": map[string]interface{}{"message": "CN gateway returned empty content", "code": 502},
			}))
			w.Flush()
		}

		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			UpdateRequestLogResponse(logID, map[string]interface{}{"streamed_content": fullContent.String()})
			UpdateRequestLogRawLines(logID, rawLines)
		}

		sys, prompt := extractSystemAndPrompt(req.Messages)
		um.Record(req.Model, countTokens(sys+"\n"+prompt), countTokens(fullThinking.String()+fullContent.String()), false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{
			InputTokens:    countTokens(sys + "\n" + prompt),
			OutputTokens:   countTokens(fullThinking.String() + fullContent.String()),
			ThinkingTokens: countTokens(fullThinking.String()),
		})
	})

	return false
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return strconv.Quote(err.Error())
	}
	return string(b)
}

// ── CN gateway transport, Anthropic protocol ────────────────────────────────
//
// HandleAnthropicChat/handleAnthropicChatCN/handleAnthropicStreamCN mirror
// HandleChat/handleChatCN/handleStreamCN above, but speak the Anthropic
// /v1/messages wire format on both sides instead of OpenAI chat-completions.
// The CN gateway itself is never told about this — it only ever sees the
// OpenAI-shape ChatRequest body built by anthropicRequestToChatRequest, and
// its responses are re-packaged into Anthropic response/SSE shapes using the
// same buildAnthropicResponse/buildAnthropicStartEvent helpers the CLI path
// already relies on. Tool calls ride the existing prompt-injection protocol
// (buildToolSystemPromptFromRaw + parseToolCallOutput) rather than the
// gateway's native tool-calling fields, matching the CLI path's behavior.

// HandleAnthropicChat is the CN-gateway direct-API entry point for the
// Anthropic /v1/messages protocol. Only called when cfg.Backend == "cn" —
// the caller (handleAnthropicMessages) is responsible for that check, same
// as it already logs-and-skips for other backends.
func (c *DirectClient) HandleAnthropicChat(ctx *fasthttp.RequestCtx, req AnthropicRequest, um *UsageManager) bool {
	started := time.Now()
	if req.Stream {
		return c.handleAnthropicStreamCN(ctx, req, um, started)
	}
	return c.handleAnthropicChatCN(ctx, req, um, started)
}

func (c *DirectClient) handleAnthropicChatCN(ctx *fasthttp.RequestCtx, req AnthropicRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()

	toolPrompt := ""
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, true)
	}
	chatReq := anthropicRequestToChatRequest(req, toolPrompt)

	bodyJSON, modelKey, source, err := buildCNRequestBody(chatReq, cfg)
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

	var contentBuilder strings.Builder
	var thinkingBuilder strings.Builder
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
		if err := json.Unmarshal(chunkJSON, &chunk); err == nil {
			for _, choice := range chunk.Choices {
				contentBuilder.WriteString(choice.Content())
				thinkingBuilder.WriteString(choice.ReasoningContent())
			}
		}
	}

	finalContent := contentBuilder.String()
	finalThinking := thinkingBuilder.String()
	if finalContent == "" {
		dump := strings.Join(rawLines, " | ")
		if len(dump) > 2000 {
			dump = dump[:2000]
		}
		AddSystemLog(fmt.Sprintf("CN gateway produced empty content, raw SSE lines: %s", dump), "warn", "direct")
		AddSystemLog("Falling back to CLI mode due to empty CN gateway response", "warn", "direct")
		return true
	}

	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	sys, prompt := extractSystemAndPrompt(chatReq.Messages)
	inputTokens := countTokens(sys + "\n" + prompt)
	parsed := parseToolCallOutput(finalContent)
	respData := buildAnthropicResponse(id, req.Model, finalContent, parsed, inputTokens, finalThinking)

	ctx.SetStatusCode(http.StatusOK)
	ctx.SetContentType("application/json")
	json.NewEncoder(ctx).Encode(respData)
	ctx.SetUserValue("response_body", respData)

	um.Record(req.Model, inputTokens, countTokens(finalThinking+finalContent), false, time.Since(started).Milliseconds())
	ctx.SetUserValue("log_metrics", &LogMetrics{
		InputTokens:    inputTokens,
		OutputTokens:   countTokens(finalThinking + finalContent),
		ThinkingTokens: countTokens(finalThinking),
	})

	if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
		UpdateRequestLogRawLines(logID, rawLines)
	}

	return false
}

func (c *DirectClient) handleAnthropicStreamCN(ctx *fasthttp.RequestCtx, req AnthropicRequest, um *UsageManager, started time.Time) bool {
	cfg := c.Config.Get()

	toolPrompt := ""
	if len(req.Tools) > 0 {
		toolPrompt = buildToolSystemPromptFromRaw(req.Tools, true)
	}
	chatReq := anthropicRequestToChatRequest(req, toolPrompt)

	bodyJSON, modelKey, source, err := buildCNRequestBody(chatReq, cfg)
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

	ctx.SetContentType("text/event-stream")
	ctx.Response.Header.Set("Cache-Control", "no-cache")
	ctx.Response.Header.Set("Connection", "keep-alive")
	ctx.Response.Header.Set("Transfer-Encoding", "chunked")
	ctx.SetStatusCode(http.StatusOK)

	sys, prompt := extractSystemAndPrompt(chatReq.Messages)
	inputTokens := countTokens(sys + "\n" + prompt)
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
		defer hResp.Body.Close()

		writeEvent := func(eventType string, data interface{}) {
			d, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, d)
			w.Flush()
		}

		writeEvent("message_start", buildAnthropicStartEvent(id, req.Model, inputTokens))

		scanner := bufio.NewScanner(hResp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

		classifier := &streamClassifier{}
		nextIndex := 0
		thinkingOpen, thinkingIndex := false, 0
		textOpen, textIndex := false, 0
		var resolvedTools *ParsedToolOutput
		var fullContent, fullThinking strings.Builder
		var rawLines []string

		closeThinkingIfOpen := func() {
			if thinkingOpen {
				writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: thinkingIndex})
				thinkingOpen = false
			}
		}
		openTextIfNeeded := func() {
			if !textOpen {
				textIndex = nextIndex
				nextIndex++
				writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: textIndex, ContentBlock: &AnthropicContent{Type: "text", Text: ""}})
				textOpen = true
			}
		}

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
				writeEvent("error", map[string]interface{}{
					"type":  "error",
					"error": map[string]interface{}{"type": "api_error", "message": errMsg},
				})
				// Stream already began with a 200 response — can't switch to an
				// HTTP error status now, just end the stream after the error event.
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
					// Already emitted the tool_use blocks; keep draining to EOF
					// so the gateway response body is fully consumed, but stop
					// forwarding/parsing (mirrors the CLI stream handler).
					fullContent.WriteString(choice.Content())
					fullThinking.WriteString(choice.ReasoningContent())
					continue
				}

				thinking := choice.ReasoningContent()
				if thinking != "" {
					fullThinking.WriteString(thinking)
					if !thinkingOpen {
						thinkingIndex = nextIndex
						nextIndex++
						writeEvent("content_block_start", AnthropicSSEEvent{Type: "content_block_start", Index: thinkingIndex, ContentBlock: &AnthropicContent{Type: "thinking", Thinking: ""}})
						thinkingOpen = true
					}
					writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: thinkingIndex, Delta: &AnthropicEventDelta{Type: "thinking_delta", Thinking: thinking}})
				}

				content := choice.Content()
				if content == "" {
					continue
				}
				fullContent.WriteString(content)

				plainText, resolved := classifier.feed(content)
				if plainText != "" {
					closeThinkingIfOpen()
					openTextIfNeeded()
					writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: textIndex, Delta: &AnthropicEventDelta{Type: "text_delta", Text: plainText}})
				}

				if resolved != nil {
					closeThinkingIfOpen()
					if textOpen {
						writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: textIndex})
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
					resolvedTools = resolved
					// Do not break — keep draining the scanner to EOF below.
				}
			}
		}

		thinkingStr := fullThinking.String()

		if resolvedTools == nil {
			if remaining := classifier.flush(); remaining != "" {
				closeThinkingIfOpen()
				openTextIfNeeded()
				writeEvent("content_block_delta", AnthropicSSEEvent{Type: "content_block_delta", Index: textIndex, Delta: &AnthropicEventDelta{Type: "text_delta", Text: remaining}})
			}
			closeThinkingIfOpen()
			openTextIfNeeded()
			writeEvent("content_block_stop", AnthropicSSEEvent{Type: "content_block_stop", Index: textIndex})
			writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "end_turn"}})
			writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		} else {
			writeEvent("message_delta", AnthropicSSEEvent{Type: "message_delta", Delta: &AnthropicEventDelta{Type: "message_delta", StopReason: "tool_use"}})
			writeEvent("message_stop", AnthropicSSEEvent{Type: "message_stop"})
		}

		if fullContent.Len() == 0 {
			dump := strings.Join(rawLines, " | ")
			if len(dump) > 2000 {
				dump = dump[:2000]
			}
			AddSystemLog(fmt.Sprintf("CN gateway stream produced empty content, raw SSE lines: %s", dump), "warn", "direct")
		}

		if logID, ok := ctx.UserValue("log_id").(string); ok && logID != "" {
			parsed := resolvedTools
			if parsed == nil {
				parsed = parseToolCallOutput(fullContent.String())
			}
			respData := buildAnthropicResponse(id, req.Model, fullContent.String(), parsed, inputTokens, thinkingStr)
			UpdateRequestLogResponse(logID, respData)
			UpdateRequestLogRawLines(logID, rawLines)
		}

		um.Record(req.Model, inputTokens, countTokens(thinkingStr+fullContent.String()), false, time.Since(started).Milliseconds())
		ctx.SetUserValue("log_metrics", &LogMetrics{
			InputTokens:    inputTokens,
			OutputTokens:   countTokens(thinkingStr + fullContent.String()),
			ThinkingTokens: countTokens(thinkingStr),
		})
	})

	return false
}
