package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// QuotaBucket mirrors one usage bucket (plan quota or add-on quota) from the
// real Qoder quota/usage API response.
type QuotaBucket struct {
	Total      float64 `json:"total"`
	Used       float64 `json:"used"`
	Remaining  float64 `json:"remaining"`
	Percentage float64 `json:"percentage"`
	Unit       string  `json:"unit"`
	DetailURL  string  `json:"detailUrl,omitempty"`
}

// QuotaInfo mirrors the real GET .../api/v2/quota/usage response schema,
// confirmed via mitm capture against the CN gateway.
type QuotaInfo struct {
	UserID               string      `json:"userId"`
	UserType             string      `json:"userType"`
	UsageType            string      `json:"usageType"`
	TotalUsagePercentage float64     `json:"totalUsagePercentage"`
	IsQuotaExceeded      bool        `json:"isQuotaExceeded"`
	ExpiresAt            int64       `json:"expiresAt"`
	UpgradeURL           string      `json:"upgradeUrl"`
	UserQuota            QuotaBucket `json:"userQuota"`
	AddOnQuota           QuotaBucket `json:"addOnQuota"`
	IsPlanQuotaProrated  bool        `json:"isPlanQuotaProrated"`
}

// QuotaManager caches the last fetched QuotaInfo per active account to avoid
// hammering the real backend on every dashboard poll.
type QuotaManager struct {
	mu        sync.Mutex
	cached    *QuotaInfo
	cachedFor string
	fetchedAt time.Time
	ttl       time.Duration
}

func NewQuotaManager() *QuotaManager {
	return &QuotaManager{ttl: 60 * time.Second}
}

// Get returns cached quota info for the currently active account, refetching
// from the real backend when the cache is stale, force is set, or the active
// account changed since the last fetch.
func (qm *QuotaManager) Get(cm *ConfigManager, force bool) (*QuotaInfo, error) {
	cfg := cm.Get()
	accountKey := cfg.UserID + "|" + cfg.MachineID + "|" + cfg.Token

	qm.mu.Lock()
	if !force && qm.cached != nil && qm.cachedFor == accountKey && time.Since(qm.fetchedAt) < qm.ttl {
		cached := qm.cached
		qm.mu.Unlock()
		return cached, nil
	}
	qm.mu.Unlock()

	// Real desktop client hits openapi.qoder.com.cn with a plain Bearer token
	// for quota/usage on both backends (confirmed via mitm capture) — the
	// gateway.*/algo/ signed path is not used for this endpoint.
	oc := NewOAuthClient(cfg.Backend)
	info, err := oc.GetQuotaUsage(context.Background(), cfg.Token)
	if err != nil {
		return nil, err
	}

	qm.mu.Lock()
	qm.cached = info
	qm.cachedFor = accountKey
	qm.fetchedAt = time.Now()
	qm.mu.Unlock()

	return info, nil
}

// GetQuotaUsage fetches quota/usage from the global backend's simple-Bearer
// endpoint. Mirrors GetUserInfo's host-swap pattern.
func (c *OAuthClient) GetQuotaUsage(ctx context.Context, token string) (*QuotaInfo, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	openAPIHost := c.OpenAPIHost
	if strings.Contains(openAPIHost, "qoder.com.cn") {
		openAPIHost = "openapi.qoder.com.cn"
	}
	quotaURL := "https://" + openAPIHost + "/api/v2/quota/usage"

	req, err := http.NewRequestWithContext(ctx, "GET", quotaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("quota endpoint returned %d: %s", resp.StatusCode, string(body))
	}

	var info QuotaInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse quota response: %w", err)
	}
	return &info, nil
}
