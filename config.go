package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

type Model struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Tier        string `json:"tier"`
	Description string `json:"description"`
}

// Account holds credentials for a single logged-in Qoder identity.
type Account struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Backend          string   `json:"backend"`
	Token            string   `json:"token"`
	UseDirectAPI     bool     `json:"use_direct_api"`
	ProxyURL         string   `json:"proxy_url"`
	UserID           string   `json:"user_id,omitempty"`
	RefreshToken     string   `json:"refresh_token,omitempty"`
	ExpireTime       int64    `json:"expire_time,omitempty"`
	OrganizationID   string   `json:"organization_id,omitempty"`
	OrganizationTags []string `json:"organization_tags,omitempty"`
	DataPolicyAgreed bool     `json:"data_policy_agreed,omitempty"`
	MachineID        string   `json:"machine_id,omitempty"`
}

// Config is the backwards-compatible single-account view handed out by Get()
// and accepted by Update(). All external call-sites keep compiling untouched.
type Config struct {
	Backend          string   `json:"backend"`
	Token            string   `json:"token"`
	UseDirectAPI     bool     `json:"use_direct_api"`
	ProxyURL         string   `json:"proxy_url"`
	Models           []Model  `json:"models"`
	UserID           string   `json:"user_id,omitempty"`
	RefreshToken     string   `json:"refresh_token,omitempty"`
	ExpireTime       int64    `json:"expire_time,omitempty"`
	OrganizationID   string   `json:"organization_id,omitempty"`
	OrganizationTags []string `json:"organization_tags,omitempty"`
	DataPolicyAgreed bool     `json:"data_policy_agreed,omitempty"`
	MachineID        string   `json:"machine_id,omitempty"`
}

// configFile is the on-disk format (accounts array + shared models).
type configFile struct {
	Accounts        []Account `json:"accounts"`
	ActiveAccountID string    `json:"active_account_id"`
	Models          []Model   `json:"models"`
}

// AccountSummary is the read-only view sent to the dashboard frontend.
type AccountSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Backend     string `json:"backend"`
	MaskedToken string `json:"maskedToken"`
	HasToken    bool   `json:"hasToken"`
	UserID      string `json:"userId,omitempty"`
	Active      bool   `json:"active"`
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
	cfg  configFile
}

func NewConfigManager(path string) (*ConfigManager, error) {
	cm := &ConfigManager{path: path}
	if err := cm.Load(); err != nil {
		return nil, err
	}
	return cm, nil
}

// migrateLegacy wraps a flat Config into a single-account configFile.
func migrateLegacy(old Config) configFile {
	return configFile{
		Accounts: []Account{
			{
				ID:               "default",
				Name:             "Default",
				Backend:          old.Backend,
				Token:            old.Token,
				UseDirectAPI:     old.UseDirectAPI,
				ProxyURL:         old.ProxyURL,
				UserID:           old.UserID,
				RefreshToken:     old.RefreshToken,
				ExpireTime:       old.ExpireTime,
				OrganizationID:   old.OrganizationID,
				OrganizationTags: old.OrganizationTags,
				DataPolicyAgreed: old.DataPolicyAgreed,
				MachineID:        old.MachineID,
			},
		},
		ActiveAccountID: "default",
		Models:          old.Models,
	}
}

func (cm *ConfigManager) Load() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	dir := filepath.Dir(cm.path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.MkdirAll(dir, 0755)
	}

	if _, err := os.Stat(cm.path); os.IsNotExist(err) {
		cm.cfg = configFile{
			Accounts: []Account{
				{
					ID:        "default",
					Name:      "Default",
					Backend:   getEnv("CLI_BACKEND", "global"),
					Token:     getEnv("QODERCN_PERSONAL_ACCESS_TOKEN", getEnv("QODER_PERSONAL_ACCESS_TOKEN", getEnv("QODER_API_KEY", ""))),
					MachineID: uuid.New().String(),
				},
			},
			ActiveAccountID: "default",
			Models:          DefaultModels,
		}
		return cm.saveNoLock()
	}

	data, err := os.ReadFile(cm.path)
	if err != nil {
		return err
	}

	// Detect format: raw map keys reveal whether file holds the old flat Config
	// or the new configFile (which has an "accounts" key).
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err == nil && probe["accounts"] != nil {
		// New format.
		if err := json.Unmarshal(data, &cm.cfg); err != nil {
			return err
		}
		return nil
	}

	// Legacy flat format — migrate in place.
	var old Config
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}
	cm.cfg = migrateLegacy(old)
	return cm.saveNoLock()
}

func (cm *ConfigManager) activeIndex() int {
	for i, a := range cm.cfg.Accounts {
		if a.ID == cm.cfg.ActiveAccountID {
			return i
		}
	}
	if len(cm.cfg.Accounts) > 0 {
		return 0
	}
	return -1
}

func (cm *ConfigManager) activeAccount() *Account {
	idx := cm.activeIndex()
	if idx < 0 {
		return nil
	}
	return &cm.cfg.Accounts[idx]
}

func (cm *ConfigManager) Get() Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	a := cm.activeAccount()
	cfg := Config{
		Backend:          a.Backend,
		Token:            a.Token,
		UseDirectAPI:     a.UseDirectAPI,
		ProxyURL:         a.ProxyURL,
		UserID:           a.UserID,
		RefreshToken:     a.RefreshToken,
		ExpireTime:       a.ExpireTime,
		OrganizationID:   a.OrganizationID,
		OrganizationTags: a.OrganizationTags,
		DataPolicyAgreed: a.DataPolicyAgreed,
		MachineID:        a.MachineID,
	}
	cfg.Models = append([]Model(nil), cm.cfg.Models...)
	return cfg
}

func (cm *ConfigManager) Update(newCfg Config) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	a := cm.activeAccount()
	if a == nil {
		// No accounts yet — create one.
		a = &Account{
			ID:        "default",
			Name:      "Default",
			MachineID: uuid.New().String(),
		}
		cm.cfg.Accounts = []Account{*a}
		cm.cfg.ActiveAccountID = a.ID
	}
	a.Backend = newCfg.Backend
	a.Token = newCfg.Token
	a.UseDirectAPI = newCfg.UseDirectAPI
	a.ProxyURL = newCfg.ProxyURL
	a.UserID = newCfg.UserID
	a.RefreshToken = newCfg.RefreshToken
	a.ExpireTime = newCfg.ExpireTime
	a.OrganizationID = newCfg.OrganizationID
	a.OrganizationTags = newCfg.OrganizationTags
	a.DataPolicyAgreed = newCfg.DataPolicyAgreed
	a.MachineID = newCfg.MachineID
	cm.cfg.Models = newCfg.Models
	return cm.saveNoLock()
}

func (cm *ConfigManager) saveNoLock() error {
	data, err := json.MarshalIndent(cm.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cm.path, data, 0644)
}

func (cm *ConfigManager) ListAccounts() []AccountSummary {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	out := make([]AccountSummary, 0, len(cm.cfg.Accounts))
	for _, a := range cm.cfg.Accounts {
		out = append(out, AccountSummary{
			ID:          a.ID,
			Name:        a.Name,
			Backend:     a.Backend,
			MaskedToken: maskToken(a.Token),
			HasToken:    a.Token != "",
			UserID:      a.UserID,
			Active:      a.ID == cm.cfg.ActiveAccountID,
		})
	}
	return out
}

func (cm *ConfigManager) AddAccount(name, backend string) (AccountSummary, error) {
	if name == "" {
		return AccountSummary{}, nil
	}
	id := uuid.New().String()
	cm.mu.Lock()
	defer cm.mu.Unlock()
	a := Account{
		ID:        id,
		Name:      name,
		Backend:   backend,
		MachineID: uuid.New().String(),
	}
	cm.cfg.Accounts = append(cm.cfg.Accounts, a)
	cm.cfg.ActiveAccountID = id
	if err := cm.saveNoLock(); err != nil {
		return AccountSummary{}, err
	}
	return AccountSummary{
		ID:      a.ID,
		Name:    a.Name,
		Backend: a.Backend,
		Active:  true,
	}, nil
}

func (cm *ConfigManager) SetActiveAccount(id string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	for _, a := range cm.cfg.Accounts {
		if a.ID == id {
			cm.cfg.ActiveAccountID = id
			return cm.saveNoLock()
		}
	}
	return nil
}

func (cm *ConfigManager) RemoveAccount(id string) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	newAccs := make([]Account, 0, len(cm.cfg.Accounts)-1)
	found := false
	for _, a := range cm.cfg.Accounts {
		if a.ID == id {
			found = true
			continue
		}
		newAccs = append(newAccs, a)
	}
	if !found {
		return nil
	}
	// Cannot delete last account.
	if len(newAccs) == 0 {
		return nil
	}
	cm.cfg.Accounts = newAccs
	if cm.cfg.ActiveAccountID == id {
		cm.cfg.ActiveAccountID = newAccs[0].ID
	}
	return cm.saveNoLock()
}

func maskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) > 8 {
		return t[:6] + "..." + t[len(t)-4:]
	}
	return "******"
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
