package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/fasthttp/router"
	"github.com/valyala/fasthttp"
)

var startTime time.Time

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

	r := router.New()

	// ── Public Routes ────────────────────────────────────────────────────────────
	r.GET("/", func(ctx *fasthttp.RequestCtx) {
		ctx.SetContentType("application/json")
		fmt.Fprintf(ctx, `{"name":"Qoder Go Proxy","version":"3.2.15","dashboard":"/dashboard/"}`)
	})

	r.GET("/health", func(ctx *fasthttp.RequestCtx) {
		ctx.SetStatusCode(200)
		fmt.Fprintf(ctx, "ok")
	})

	// ── OpenAI & Anthropic Routes ───────────────────────────────────────────────
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
		}
	}

	fmt.Printf("🚀 Qoder Go Proxy starting on :%s\n", port)
	AddSystemLog("Qoder Proxy starting...", "info", "system")
	if err := fasthttp.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("Error in ListenAndServe: %s", err)
	}
}
