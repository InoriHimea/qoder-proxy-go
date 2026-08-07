package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type UsageRecord struct {
	Timestamp  string `json:"timestamp"`
	Model      string `json:"model"`
	InputLen   int    `json:"input_length"`
	OutputLen  int    `json:"output_length"`
	IsError    bool   `json:"is_error"`
	DurationMs int64  `json:"duration_ms"`
}

type UsageStats struct {
	TotalRequests   int            `json:"total_requests"`
	TotalErrors     int            `json:"total_errors"`
	RequestsByModel map[string]int `json:"requests_by_model"`
	ModelTiers      map[string]string `json:"model_tiers,omitempty"`
	RecentRecords   []UsageRecord  `json:"recent_records"`
}

type UsageManager struct {
	path  string
	mu    sync.RWMutex
	stats UsageStats
}

func NewUsageManager(path string) (*UsageManager, error) {
	um := &UsageManager{
		path: path,
		stats: UsageStats{
			RequestsByModel: make(map[string]int),
			RecentRecords:   make([]UsageRecord, 0),
		},
	}
	um.Load() // Ignore error on load, just start fresh if it fails
	return um, nil
}

func (um *UsageManager) Load() error {
	um.mu.Lock()
	defer um.mu.Unlock()

	dir := filepath.Dir(um.path)
	os.MkdirAll(dir, 0755)

	data, err := os.ReadFile(um.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &um.stats)
}

func (um *UsageManager) SetModelTiers(models []Model) {
	um.mu.Lock()
	defer um.mu.Unlock()
	if um.stats.ModelTiers == nil {
		um.stats.ModelTiers = make(map[string]string)
	} else {
		clear(um.stats.ModelTiers)
	}
	for _, m := range models {
		um.stats.ModelTiers[m.ID] = m.Tier
	}
}

func (um *UsageManager) saveNoLock() error {
	data, err := json.MarshalIndent(um.stats, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(um.path, data, 0644)
}

func (um *UsageManager) Record(model string, inputLen, outputLen int, isError bool, durationMs int64) {
	um.mu.Lock()
	defer um.mu.Unlock()

	um.stats.TotalRequests++
	if isError {
		um.stats.TotalErrors++
	}

	if um.stats.RequestsByModel == nil {
		um.stats.RequestsByModel = make(map[string]int)
	}
	um.stats.RequestsByModel[model]++

	record := UsageRecord{
		Timestamp:  time.Now().Format(time.RFC3339),
		Model:      model,
		InputLen:   inputLen,
		OutputLen:  outputLen,
		IsError:    isError,
		DurationMs: durationMs,
	}

	um.stats.RecentRecords = append([]UsageRecord{record}, um.stats.RecentRecords...)
	if len(um.stats.RecentRecords) > 100 {
		um.stats.RecentRecords = um.stats.RecentRecords[:100] // Keep last 100
	}

	// Save asynchronously to not block the request
	go func() {
		um.mu.Lock()
		defer um.mu.Unlock()
		um.saveNoLock()
	}()
}

func (um *UsageManager) Get() UsageStats {
	um.mu.RLock()
	defer um.mu.RUnlock()
	return um.stats
}

func (um *UsageManager) Reset() {
	um.mu.Lock()
	defer um.mu.Unlock()
	um.stats = UsageStats{
		RequestsByModel: make(map[string]int),
		RecentRecords:   make([]UsageRecord, 0),
	}
	um.saveNoLock()
}
