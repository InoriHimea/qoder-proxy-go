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

	maskedToken := maskToken(cfg.Token)

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

	next := cm.Get()
	next.Backend = input.Backend
	next.UseDirectAPI = input.UseDirectApi
	next.ProxyURL = input.ProxyUrl
	next.Models = input.Models

	// Logic to avoid overwriting with masked token
	if !strings.Contains(input.Token, "...") && input.Token != "******" && input.Token != "" {
		next.Token = input.Token
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
		"version":    serverVersion + "-go",
	}
	json.NewEncoder(ctx).Encode(resp)
}

func handleConfig(ctx *fasthttp.RequestCtx) {
	resp := map[string]interface{}{
		"publicBaseUrl": fmt.Sprintf("http://%s", ctx.Host()),
		"version":       serverVersion + "-go",
	}
	json.NewEncoder(ctx).Encode(resp)
}

func handleOAuthStatus(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	cfg := cm.Get()

	resp := map[string]interface{}{
		"tokenType":  "",
		"userID":     cfg.UserID,
		"expireTime": cfg.ExpireTime,
		"hasRefresh": cfg.RefreshToken != "",
	}

	if cfg.Token == "" {
		resp["loggedIn"] = false
	} else if strings.HasPrefix(cfg.Token, "dt-") {
		resp["loggedIn"] = true
		resp["tokenType"] = "device_token"
	} else if strings.HasPrefix(cfg.Token, "pt-") || strings.HasPrefix(cfg.Token, "qodercn-") {
		resp["loggedIn"] = true
		resp["tokenType"] = "personal_access_token"
	} else {
		resp["loggedIn"] = true
		resp["tokenType"] = "unknown"
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

func handleListAccounts(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	json.NewEncoder(ctx).Encode(map[string]interface{}{
		"accounts": cm.ListAccounts(),
	})
}

func handleAddAccount(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	var input struct {
		Name    string `json:"name"`
		Backend string `json:"backend"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &input); err != nil {
		ctx.Error("Invalid JSON", http.StatusBadRequest)
		return
	}
	if input.Name == "" {
		ctx.Error("name is required", http.StatusBadRequest)
		return
	}
	if input.Backend == "" {
		input.Backend = "global"
	}
	summary, err := cm.AddAccount(input.Name, input.Backend)
	if err != nil {
		ctx.Error(err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(ctx).Encode(summary)
}

func handleActivateAccount(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	id, _ := ctx.UserValue("id").(string)
	if err := cm.SetActiveAccount(id); err != nil {
		ctx.Error(err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(ctx).Encode(map[string]bool{"ok": true})
}

func handleRemoveAccount(ctx *fasthttp.RequestCtx, cm *ConfigManager) {
	id, _ := ctx.UserValue("id").(string)
	if err := cm.RemoveAccount(id); err != nil {
		ctx.Error(err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(ctx).Encode(map[string]bool{"ok": true})
}

func handleQuotaUsage(ctx *fasthttp.RequestCtx, cm *ConfigManager, dc *DirectClient, qm *QuotaManager) {
	force := string(ctx.QueryArgs().Peek("force")) == "true"
	info, err := qm.Get(cm, dc, force)
	if err != nil {
		ctx.SetStatusCode(http.StatusBadGateway)
		json.NewEncoder(ctx).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(ctx).Encode(info)
}
