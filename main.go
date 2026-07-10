package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

var startTime time.Time

const serverVersion = "3.9.0"

func main() {
	startTime = time.Now()

	port := getEnv("PORT", "3000")
	configPath := getEnv("CONFIG_FILE_PATH", "data/config.json")
	usagePath := getEnv("USAGE_FILE_PATH", "data/usage.json")

	cm, err := NewConfigManager(configPath)
	if err != nil {
		log.Fatalf("Failed to initialize config manager: %v", err)
	}

	um, err := NewUsageManager(usagePath)
	if err != nil {
		log.Fatalf("Failed to initialize usage manager: %v", err)
	}

	logDBPath := getEnv("LOG_DB_PATH", "data/logs.db")
	if err := InitLogDB(logDBPath); err != nil {
		log.Printf("Failed to initialize log database: %v", err)
	}

	dc := NewDirectClient(cm)
	qm := NewQuotaManager()

	r := router.New()

	r.POST("/oauth/login", func(ctx *fasthttp.RequestCtx) {
		oc := NewOAuthClient(cm.Get().Backend)
		sessionID, authURL, err := oc.CreateLoginSession()
		if err != nil {
			ctx.SetStatusCode(500)
			json.NewEncoder(ctx).Encode(map[string]interface{}{
				"error": fmt.Sprintf("failed to create OAuth session: %v", err),
			})
			return
		}
		json.NewEncoder(ctx).Encode(map[string]interface{}{
			"session_id": sessionID,
			"auth_url":   authURL,
			"message":    "Open the auth URL in your browser to login, then GET /oauth/session/<session_id> for result.",
		})
	})
	r.GET("/oauth/session/{session_id}", func(ctx *fasthttp.RequestCtx) {
		sessionID := ctx.UserValue("session_id").(string)
		oc := NewOAuthClient(cm.Get().Backend)
		result, err := oc.GetSessionResult(sessionID, 300)
		if err != nil {
			ctx.SetStatusCode(500)
			json.NewEncoder(ctx).Encode(map[string]interface{}{"error": err.Error()})
			return
		}
		if result.Error == "" && result.Token != "" {
			cfg := cm.Get()
			cfg.Token = result.Token
			if result.RefreshToken != "" {
				cfg.RefreshToken = result.RefreshToken
			}
			if result.UserID != "" {
				cfg.UserID = result.UserID
			}
			if result.ExpireTime != 0 {
				cfg.ExpireTime = result.ExpireTime
			}
			if result.OrganizationID != "" {
				cfg.OrganizationID = result.OrganizationID
			}
			if result.OrganizationTags != nil {
				cfg.OrganizationTags = result.OrganizationTags
			}
			cfg.DataPolicyAgreed = result.DataPolicyAgreed
			if err := cm.Update(cfg); err != nil {
				AddSystemLog(fmt.Sprintf("Failed to persist OAuth login token: %v", err), "error", "oauth")
			} else {
				AddSystemLog("OAuth login succeeded, device token stored", "info", "oauth")
			}
		}
		json.NewEncoder(ctx).Encode(result)
	})
	r.DELETE("/oauth/logout", func(ctx *fasthttp.RequestCtx) {
		cfg := cm.Get()
		cfg.Token = ""
		cfg.UserID = ""
		cfg.RefreshToken = ""
		cfg.ExpireTime = 0
		cfg.OrganizationID = ""
		cfg.OrganizationTags = nil
		cfg.DataPolicyAgreed = false
		cm.Update(cfg)
		json.NewEncoder(ctx).Encode(map[string]interface{}{"ok": true, "message": "OAuth token cleared"})
	})

	// ── Public Routes ────────────────────────────────────────────────────────────
	r.GET("/", func(ctx *fasthttp.RequestCtx) {
		ctx.SetContentType("application/json")
		fmt.Fprintf(ctx, `{"name":"Qoder Go Proxy","version":"%s","dashboard":"/dashboard/"}`, serverVersion)
	})

	r.GET("/health", func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(200)
		fmt.Fprintf(ctx, "ok")
	})

	// ── OpenAI & Anthropic Routes ───────────────────────────────────────────────
	// Bare /v1 route: many OpenAI-compatible clients probe the base URL for
	// connectivity before sending actual requests. Without this, the probe
	// gets a 404 from the router and the client refuses to proceed.
	// NOTE: fasthttp/router's radix tree does not allow both "/v1" and "/v1/"
	// — it panics with "handler already registered". Registering "/v1" alone
	// is sufficient; the router's RedirectTrailingSlash option handles the
	// trailing-slash variant automatically.
	r.GET("/v1", func(ctx *fasthttp.RequestCtx) {
		ctx.SetContentType("application/json")
		fmt.Fprintf(ctx, `{"status":"ok","name":"Qoder Go Proxy","version":"%s","endpoints":["/v1/models","/v1/chat/completions","/v1/responses","/v1/messages"]}`, serverVersion)
	})
	r.GET("/v1/models", func(ctx *fasthttp.RequestCtx) {
		handleModels(ctx, cm)
	})
	r.POST("/v1/chat/completions", func(ctx *fasthttp.RequestCtx) {
		handleChatCompletions(ctx, cm, um, dc)

	})
	r.POST("/v/chat", func(ctx *fasthttp.RequestCtx) {
		handleChatCompletions(ctx, cm, um, dc)

	})
	r.POST("/v1/messages", func(ctx *fasthttp.RequestCtx) {
		handleAnthropicMessages(ctx, cm, um, dc)
	})
	r.POST("/v1/message", func(ctx *fasthttp.RequestCtx) {
		handleAnthropicMessages(ctx, cm, um, dc)
	})
	r.POST("/responses", func(ctx *fasthttp.RequestCtx) {
		handleChatCompletions(ctx, cm, um, dc)

	})
	r.POST("/v1/responses", func(ctx *fasthttp.RequestCtx) {
		handleChatCompletions(ctx, cm, um, dc)

	})

	// ── Usage API ────────────────────────────────────────────────────────────────
	r.GET("/usage/local", func(ctx *fasthttp.RequestCtx) {
		handleUsageLocal(ctx, um)
	})
	r.POST("/usage/reset-local", func(ctx *fasthttp.RequestCtx) {
		handleUsageReset(ctx, um)
	})

	// ── Debug API (No Auth) ──────────────────────────────────────────────────────
	r.GET("/debug/logs", handleGetSystemLogs)

	// ── Dashboard Routes ─────────────────────────────────────────────────────────
	r.GET("/dashboard/", func(ctx *fasthttp.RequestCtx) {
		fasthttp.ServeFile(ctx, "public/index.html")
	})
	r.ServeFiles("/dashboard/static/{filepath:*}", "public")

	// Dashboard APIs
	r.GET("/dashboard/api/config", handleConfig)
	r.GET("/dashboard/api/status", handleStatus)
	r.GET("/dashboard/api/settings", func(ctx *fasthttp.RequestCtx) {
		handleGetSettings(ctx, cm)
	})
	r.POST("/dashboard/api/settings", func(ctx *fasthttp.RequestCtx) {
		handlePostSettings(ctx, cm)
	})
	r.GET("/dashboard/api/models", func(ctx *fasthttp.RequestCtx) {
		handleModels(ctx, cm)
	})
	r.POST("/dashboard/api/models/refresh", func(ctx *fasthttp.RequestCtx) {
		handleModelsRefresh(ctx, cm)
	})
	r.GET("/dashboard/api/oauth/status", func(ctx *fasthttp.RequestCtx) {
		handleOAuthStatus(ctx, cm)
	})
	r.GET("/dashboard/api/accounts", func(ctx *fasthttp.RequestCtx) {
		handleListAccounts(ctx, cm)
	})
	r.POST("/dashboard/api/accounts", func(ctx *fasthttp.RequestCtx) {
		handleAddAccount(ctx, cm)
	})
	r.POST("/dashboard/api/accounts/{id}/activate", func(ctx *fasthttp.RequestCtx) {
		handleActivateAccount(ctx, cm)
	})
	r.DELETE("/dashboard/api/accounts/{id}", func(ctx *fasthttp.RequestCtx) {
		handleRemoveAccount(ctx, cm)
	})
	r.GET("/dashboard/api/quota", func(ctx *fasthttp.RequestCtx) {
		handleQuotaUsage(ctx, cm, qm)
	})
	r.GET("/dashboard/api/logs", handleGetRequestLogs)
	r.GET("/dashboard/api/logs/{id}", handleGetRequestLogDetail)
	r.DELETE("/dashboard/api/logs", handleClearRequestLogs)
	r.GET("/dashboard/api/logs/system", handleGetSystemLogs)
	r.DELETE("/dashboard/api/logs/system", handleClearSystemLogs)

	// ── Dashboard Authentication ──────────────────────────────────────────────────
	r.POST("/dashboard/login", func(ctx *fasthttp.RequestCtx) {
		pwd := string(ctx.FormValue("password"))
		expectedPwd := getEnv("DASHBOARD_PASSWORD", "")
		if expectedPwd != "" && pwd == expectedPwd {
			var cookie fasthttp.Cookie
			cookie.SetKey("qoder_dash_token")
			cookie.SetValue("authenticated")
			cookie.SetPath("/dashboard")
			cookie.SetMaxAge(86400 * 30) // 30 days
			ctx.Response.Header.SetCookie(&cookie)
			ctx.Redirect("/dashboard/", fasthttp.StatusFound)
			return
		}
		ctx.Redirect("/dashboard/login?error=1", fasthttp.StatusFound)
	})

	r.GET("/dashboard/logout", func(ctx *fasthttp.RequestCtx) {
		var cookie fasthttp.Cookie
		cookie.SetKey("qoder_dash_token")
		cookie.SetValue("")
		cookie.SetPath("/dashboard")
		cookie.SetMaxAge(-1)
		ctx.Response.Header.SetCookie(&cookie)
		ctx.Redirect("/dashboard/login", fasthttp.StatusFound)
	})

	r.GET("/dashboard/login", func(ctx *fasthttp.RequestCtx) {
		fasthttp.ServeFile(ctx, "public/index.html")
	})

	// Logging & Auth Middleware
	handler := func(ctx *fasthttp.RequestCtx) {
		path := string(ctx.Path())

		// Dashboard Auth Wall
		isDashboard := strings.HasPrefix(path, "/dashboard")
		isLogin := path == "/dashboard/login"
		isStatic := strings.HasPrefix(path, "/dashboard/static/")

		if isDashboard && !isLogin && !isStatic {
			expectedPwd := getEnv("DASHBOARD_PASSWORD", "")
			if expectedPwd != "" {
				cookie := string(ctx.Request.Header.Cookie("qoder_dash_token"))
				if cookie != "authenticated" {
					ctx.Redirect("/dashboard/login", fasthttp.StatusFound)
					return
				}
			}
		}

		isChatPath := path == "/v1/chat/completions" || path == "/v/chat" || path == "/responses" || path == "/v1/responses"
		isMsgPath := path == "/v1/messages" || path == "/v1/message"

		logID := ""
		if isChatPath || isMsgPath {
			logID = fmt.Sprintf("log_%d", time.Now().UnixNano())
			ctx.SetUserValue("log_id", logID)
		}

		r.Handler(ctx)

		if isChatPath || isMsgPath {
			var bodyObj map[string]interface{}
			json.Unmarshal(ctx.PostBody(), &bodyObj)

			isSSE := false
			if streamVal, ok := bodyObj["stream"].(bool); ok {
				isSSE = streamVal
			}

			respBody := ctx.UserValue("response_body")

			AddRequestLogWithID(logID, string(ctx.Method()), path, ctx.Response.StatusCode(), isSSE, bodyObj, respBody)

			if metrics, ok := ctx.UserValue("log_metrics").(*LogMetrics); ok && metrics != nil {
				UpdateRequestLogTokens(logID, metrics.InputTokens, metrics.OutputTokens, metrics.ThinkingTokens, metrics.CacheCreationTokens, metrics.CacheReadTokens)
			}
		}
	}

	fmt.Printf("🚀 Qoder Go Proxy starting on :%s\n", port)
	AddSystemLog("Qoder Proxy starting...", "info", "system")

	// ── OAuth Token Refresh Goroutine ──────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		var refreshingMu sync.Mutex

		for range ticker.C {
			cfg := cm.Get()
			if !strings.HasPrefix(cfg.Token, "dt-") || cfg.ExpireTime == 0 || cfg.RefreshToken == "" {
				continue
			}

			refreshingMu.Lock()
			now := time.Now().Unix()
			if cfg.ExpireTime-now <= 1800 { // 30 minutes
				refreshed := dc.refreshDeviceTokenViaOAuth(cfg.Token, cfg.Backend, cfg.RefreshToken)
				if refreshed != cfg.Token {
					AddSystemLog("Background OAuth token refresh succeeded", "info", "oauth")
				} else {
					AddSystemLog("Background OAuth token refresh unchanged or failed", "warn", "oauth")
				}
			}
			refreshingMu.Unlock()
		}
	}()

	if err := fasthttp.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Error in ListenAndServe: %s", err)
	}
}
