package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

type SystemLog struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Level     string `json:"level"`
	Source    string `json:"source"`
}

type RequestLog struct {
	ID           string      `json:"id"`
	Timestamp    string      `json:"timestamp"`
	Method       string      `json:"method"`
	Path         string      `json:"path"`
	StatusCode   int         `json:"statusCode"`
	IsSSE        bool        `json:"is_sse"`
	Body         interface{} `json:"body"`
	ResponseBody interface{} `json:"response_body,omitempty"`
}

var (
	systemLogs  = make([]SystemLog, 0)
	requestLogs = make([]RequestLog, 0)
	logMu       sync.RWMutex
)

func AddSystemLog(msg, level, source string) {
	logMu.Lock()
	defer logMu.Unlock()
	systemLogs = append(systemLogs, SystemLog{
		Timestamp: time.Now().Format(time.RFC3339),
		Message:   msg,
		Level:     level,
		Source:    source,
	})
	if len(systemLogs) > 500 {
		systemLogs = systemLogs[len(systemLogs)-500:]
	}
}

func AddRequestLog(method, path string, status int, isSSE bool, body interface{}, responseBody interface{}) {
	logMu.Lock()
	defer logMu.Unlock()
	requestLogs = append(requestLogs, RequestLog{
		ID:           fmt.Sprintf("log_%d", time.Now().UnixNano()),
		Timestamp:    time.Now().Format(time.RFC3339),
		Method:       method,
		Path:         path,
		StatusCode:   status,
		IsSSE:        isSSE,
		Body:         body,
		ResponseBody: responseBody,
	})
	if len(requestLogs) > 100 {
		requestLogs = requestLogs[len(requestLogs)-100:] // Keep last 100 requests to save memory
	}
}

func handleGetSystemLogs(ctx *fasthttp.RequestCtx) {
	logMu.RLock()
	defer logMu.RUnlock()
	json.NewEncoder(ctx).Encode(map[string]interface{}{"logs": systemLogs})
}

func handleGetRequestLogs(ctx *fasthttp.RequestCtx) {
	logMu.RLock()
	defer logMu.RUnlock()
	
	// Strip response body for the list view
	list := make([]RequestLog, len(requestLogs))
	for i, log := range requestLogs {
		list[i] = log
		list[i].ResponseBody = nil
	}
	
	json.NewEncoder(ctx).Encode(map[string]interface{}{"logs": list})
}

func handleGetRequestLogDetail(ctx *fasthttp.RequestCtx) {
	id := ctx.UserValue("id").(string)
	
	logMu.RLock()
	defer logMu.RUnlock()
	
	for _, log := range requestLogs {
		if log.ID == id {
			json.NewEncoder(ctx).Encode(log)
			return
		}
	}
	
	ctx.SetStatusCode(404)
}

func handleClearSystemLogs(ctx *fasthttp.RequestCtx) {
	logMu.Lock()
	systemLogs = make([]SystemLog, 0)
	logMu.Unlock()
	json.NewEncoder(ctx).Encode(map[string]interface{}{"ok": true})
}

func handleClearRequestLogs(ctx *fasthttp.RequestCtx) {
	logMu.Lock()
	requestLogs = make([]RequestLog, 0)
	logMu.Unlock()
	json.NewEncoder(ctx).Encode(map[string]interface{}{"ok": true})
}
