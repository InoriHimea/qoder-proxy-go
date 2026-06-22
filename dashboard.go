package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/valyala/fasthttp"
)

func handleGetSettings(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	cfg := cm.Get()

	maskedToken := ""
	if cfg.Token != "" {
		if len(cfg.Token) > 8 {
			maskedToken = cfg.Token[:6] + "..." + cfg.Token[len(cfg.Token)-4:]
		} else {
			maskedToken = "******"
		}
	}

	resp := map[string]interface{}{
		"backend":      cfg.Backend,
		"token":        maskedToken,
		"hasToken":     cfg.Token != "",
		"useDirectApi": cfg.UseDirectAPI,
		"proxyUrl":     cfg.ProxyURL,
		"models":       cfg.Models,
	}
	json.NewEncoder(ctx).Encode(resp)
}

func handlePostSettings(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	var input struct {
		Backend      string  `json:"backend"`
		Token        string  `json:"token"`
		UseDirectApi bool    `json:"useDirectApi"`
		ProxyUrl     string  `json:"proxyUrl"`
		Models       []Model `json:"models"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &input); err != nil {
		ctx.Error("Invalid JSON", http.StatusBadRequest)
		return
	}

	current := cm.Get()
	next := Config{
		Backend:      input.Backend,
		Token:        input.Token,
		UseDirectAPI: input.UseDirectApi,
		ProxyURL:     input.ProxyUrl,
		Models:       input.Models,
	}

	// Logic to avoid overwriting with masked token
	if strings.Contains(input.Token, "...") || input.Token == "******" || input.Token == "" {
		next.Token = current.Token
	}

	if err := cm.Update(next); err != nil {
		ctx.Error(err.Error(), http.StatusInternalServerError)
		return
	}

	ctx.SetStatusCode(http.StatusOK)
	json.NewEncoder(ctx).Encode(map[string]bool{"ok": true})
}

func handleStatus(ctx *fasthttp.RequestCtx) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	resp := map[string]interface{}{
		"status":     "ok",
		"uptime":     int(time.Since(startTime).Seconds()),
		"memoryMB":   fmt.Sprintf("%.1f", float64(m.Alloc)/1024/1024),
		"heapUsedMB": fmt.Sprintf("%.1f", float64(m.HeapAlloc)/1024/1024),
		"timestamp":  time.Now().Format(time.RFC3339),
		"version":    "3.2.16-go",
	}
	json.NewEncoder(ctx).Encode(resp)
}

func handleConfig(ctx *fasthttp.RequestCtx) {
	resp := map[string]interface{}{
		"publicBaseUrl": fmt.Sprintf("http://%s", ctx.Host()),
		"version":       "3.2.16-go",
	}
	json.NewEncoder(ctx).Encode(resp)
}

func handleUsageLocal(ctx *fasthttp.RequestCtx, um *UsageManager) {
	json.NewEncoder(ctx).Encode(um.Get())
}

func handleUsageReset(ctx *fasthttp.RequestCtx, um *UsageManager) {
	um.Reset()
	json.NewEncoder(ctx).Encode(map[string]bool{"ok": true})
}
