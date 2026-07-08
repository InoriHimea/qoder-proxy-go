package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	_ "modernc.org/sqlite"
)

type SystemLog struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Level     string `json:"level"`
	Source    string `json:"source"`
}

type RequestLog struct {
	ID                  string      `json:"id"`
	Timestamp           string      `json:"timestamp"`
	Method              string      `json:"method"`
	Path                string      `json:"path"`
	StatusCode          int         `json:"statusCode"`
	IsSSE               bool        `json:"is_sse"`
	Body                interface{} `json:"body"`
	ResponseBody        interface{} `json:"response_body,omitempty"`
	StreamRawLines      string      `json:"stream_raw_lines,omitempty"`
	InputTokens         int         `json:"input_tokens,omitempty"`
	OutputTokens        int         `json:"output_tokens,omitempty"`
	CacheCreationTokens int         `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int         `json:"cache_read_tokens,omitempty"`
	ThinkingTokens      int         `json:"thinking_tokens,omitempty"`
}

// LogMetrics holds token usage metrics for a single request.
type LogMetrics struct {
	InputTokens         int
	OutputTokens        int
	ThinkingTokens      int
	CacheCreationTokens int
	CacheReadTokens     int
}

var (
	logDB *sql.DB
	logMu sync.Mutex
)

func InitLogDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS system_logs (
			timestamp TEXT,
			message TEXT,
			level TEXT,
			source TEXT
		);
		CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY,
			timestamp TEXT,
			method TEXT,
			path TEXT,
			status_code INTEGER,
			is_sse BOOLEAN,
			body TEXT,
			response_body TEXT,
			stream_raw_lines TEXT,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_creation_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			thinking_tokens INTEGER DEFAULT 0
		);
	`)
	if err != nil {
		return err
	}

	// Migrate existing tables: add new columns if missing
	db.Exec(`ALTER TABLE request_logs ADD COLUMN stream_raw_lines TEXT`)
	db.Exec(`ALTER TABLE request_logs ADD COLUMN input_tokens INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE request_logs ADD COLUMN output_tokens INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE request_logs ADD COLUMN cache_creation_tokens INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE request_logs ADD COLUMN cache_read_tokens INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE request_logs ADD COLUMN thinking_tokens INTEGER DEFAULT 0`)

	logDB = db

	// Start cleanup goroutine
	go func() {
		for {
			CleanupOldLogs(7) // Keep 7 days
			time.Sleep(24 * time.Hour)
		}
	}()

	return nil
}

func AddSystemLog(msg, level, source string) {
	if logDB == nil {
		fmt.Printf("[%s] [%s] %s: %s\n", time.Now().Format(time.RFC3339), level, source, msg)
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = logDB.Exec("INSERT INTO system_logs (timestamp, message, level, source) VALUES (?, ?, ?, ?)",
		time.Now().Format(time.RFC3339), msg, level, source)
}

func AddRequestLogWithID(id, method, path string, status int, isSSE bool, body interface{}, responseBody interface{}) {
	if logDB == nil {
		return
	}
	if id == "" {
		id = fmt.Sprintf("log_%d", time.Now().UnixNano())
	}
	ts := time.Now().Format(time.RFC3339)
	bodyJS, _ := json.Marshal(body)
	respJS, _ := json.Marshal(responseBody)

	logMu.Lock()
	defer logMu.Unlock()
	_, _ = logDB.Exec(`INSERT INTO request_logs (id, timestamp, method, path, status_code, is_sse, body, response_body)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, ts, method, path, status, isSSE, string(bodyJS), string(respJS))
}

// UpdateRequestLogTokens updates the token metrics column for an existing log entry.
func UpdateRequestLogTokens(id string, inputTokens, outputTokens, thinkingTokens, cacheCreationTokens, cacheReadTokens int) {
	if logDB == nil || id == "" {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = logDB.Exec(`UPDATE request_logs SET input_tokens = ?, output_tokens = ?, thinking_tokens = ?, cache_creation_tokens = ?, cache_read_tokens = ? WHERE id = ?`,
		inputTokens, outputTokens, thinkingTokens, cacheCreationTokens, cacheReadTokens, id)
}

// UpdateRequestLogResponse updates the response_body column for an existing log entry.
func UpdateRequestLogResponse(id string, responseBody interface{}) {
	if logDB == nil {
		return
	}
	respJS, _ := json.Marshal(responseBody)
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = logDB.Exec("UPDATE request_logs SET response_body = ? WHERE id = ?", string(respJS), id)
}

// UpdateRequestLogRawLines stores the raw SSE lines for a streaming log entry.
func UpdateRequestLogRawLines(id string, rawLines []string) {
	if logDB == nil || id == "" {
		return
	}
	rawJS, _ := json.Marshal(rawLines)
	logMu.Lock()
	defer logMu.Unlock()
	_, _ = logDB.Exec("UPDATE request_logs SET stream_raw_lines = ? WHERE id = ?", string(rawJS), id)
}

func handleGetSystemLogs(ctx *fasthttp.RequestCtx) {
	rows, err := logDB.Query("SELECT timestamp, message, level, source FROM system_logs ORDER BY timestamp DESC LIMIT 500")
	if err != nil {
		ctx.Error(err.Error(), 500)
		return
	}
	defer rows.Close()

	logs := []SystemLog{}
	for rows.Next() {
		var l SystemLog
		rows.Scan(&l.Timestamp, &l.Message, &l.Level, &l.Source)
		logs = append(logs, l)
	}
	json.NewEncoder(ctx).Encode(map[string]interface{}{"logs": logs})
}

func handleGetRequestLogs(ctx *fasthttp.RequestCtx) {
	rows, err := logDB.Query("SELECT id, timestamp, method, path, status_code, is_sse FROM request_logs ORDER BY timestamp DESC LIMIT 200")
	if err != nil {
		ctx.Error(err.Error(), 500)
		return
	}
	defer rows.Close()

	logs := []RequestLog{}
	for rows.Next() {
		var l RequestLog
		rows.Scan(&l.ID, &l.Timestamp, &l.Method, &l.Path, &l.StatusCode, &l.IsSSE)
		logs = append(logs, l)
	}
	json.NewEncoder(ctx).Encode(map[string]interface{}{"logs": logs})
}

func handleGetRequestLogDetail(ctx *fasthttp.RequestCtx) {
	id := ctx.UserValue("id").(string)
	var l RequestLog
	var bodyStr, respStr, rawLinesStr string
	err := logDB.QueryRow("SELECT id, timestamp, method, path, status_code, is_sse, body, response_body, stream_raw_lines, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, thinking_tokens FROM request_logs WHERE id = ?", id).
		Scan(&l.ID, &l.Timestamp, &l.Method, &l.Path, &l.StatusCode, &l.IsSSE, &bodyStr, &respStr, &rawLinesStr, &l.InputTokens, &l.OutputTokens, &l.CacheCreationTokens, &l.CacheReadTokens, &l.ThinkingTokens)

	if err != nil {
		ctx.SetStatusCode(404)
		return
	}

	json.Unmarshal([]byte(bodyStr), &l.Body)
	json.Unmarshal([]byte(respStr), &l.ResponseBody)
	l.StreamRawLines = rawLinesStr

	json.NewEncoder(ctx).Encode(l)
}

func handleClearSystemLogs(ctx *fasthttp.RequestCtx) {
	_, _ = logDB.Exec("DELETE FROM system_logs")
	json.NewEncoder(ctx).Encode(map[string]interface{}{"ok": true})
}

func handleClearRequestLogs(ctx *fasthttp.RequestCtx) {
	_, _ = logDB.Exec("DELETE FROM request_logs")
	json.NewEncoder(ctx).Encode(map[string]interface{}{"ok": true})
}

func CleanupOldLogs(days int) {
	cutoff := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	_, _ = logDB.Exec("DELETE FROM system_logs WHERE timestamp < ?", cutoff)
	_, _ = logDB.Exec("DELETE FROM request_logs WHERE timestamp < ?", cutoff)
}
