package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Model struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Tier        string `json:"tier"`
	Description string `json:"description"`
}

type Config struct {
	Backend      string  `json:"backend"`
	Token        string  `json:"token"`
	UseDirectAPI bool    `json:"use_direct_api"`
	ProxyURL     string  `json:"proxy_url"`
	Models       []Model `json:"models"`
}

var DefaultModels = []Model{
	{ID: "auto", Label: "Auto (Smart Select)", Tier: "paid", Description: "Paid tier — automatically selects the best model per task."},
	{ID: "ultimate", Label: "Ultimate (Best Quality)", Tier: "paid", Description: "Paid tier — top-tier model, maximum quality."},
	{ID: "performance", Label: "Performance", Tier: "paid", Description: "Paid tier — high-performance model."},
	{ID: "qmodel_latest", Label: "Qwen3.7-Max", Tier: "new", Description: "New model — Qwen 3.7 Max (Alibaba)."},
	{ID: "qmodel", Label: "Qwen 3.6 Plus", Tier: "new", Description: "New model — Qwen 3.6 Plus (Alibaba)."},
	{ID: "kmodel", Label: "Kimi-K2.6", Tier: "new", Description: "New model — Kimi-K2.6 (Moonshot AI)."},
	{ID: "mmodel", Label: "MiniMax-M2.7", Tier: "new", Description: "New model — MiniMax-M2.7."},
	{ID: "dmodel", Label: "DeepSeek-V4-Pro", Tier: "new", Description: "New model — DeepSeek V4 Pro, reasoning-capable."},
	{ID: "dfmodel", Label: "DeepSeek-V4-Flash", Tier: "new", Description: "New model — DeepSeek V4 Flash, fast and lightweight."},
	{ID: "gm51model", Label: "GLM-5.1", Tier: "new", Description: "New model — GLM-5.1 series (Zhipu AI)."},
}

type ConfigManager struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

func NewConfigManager(path string) (*ConfigManager, error) {
	cm := &ConfigManager{path: path}
	if err := cm.Load(); err != nil {
		return nil, err
	}
	return cm, nil
}

func (cm *ConfigManager) Load() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	dir := filepath.Dir(cm.path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.MkdirAll(dir, 0755)
	}

	if _, err := os.Stat(cm.path); os.IsNotExist(err) {
		// Init with env vars or defaults
		cm.cfg = Config{
			Backend: getEnv("CLI_BACKEND", "global"),
			Token:   getEnv("QODERCN_PERSONAL_ACCESS_TOKEN", getEnv("QODER_PERSONAL_ACCESS_TOKEN", getEnv("QODER_API_KEY", ""))),
			Models:  DefaultModels,
		}
		return cm.saveNoLock()
	}

	data, err := os.ReadFile(cm.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &cm.cfg)
}

func (cm *ConfigManager) Get() Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.cfg
}

func (cm *ConfigManager) Update(newCfg Config) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.cfg = newCfg
	return cm.saveNoLock()
}

func (cm *ConfigManager) saveNoLock() error {
	data, err := json.MarshalIndent(cm.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cm.path, data, 0644)
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
