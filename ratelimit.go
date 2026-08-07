package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket. Each refill tick adds rate tokens;
// a request consumes one. Keys longer than keyPrefixLen are truncated to that
// prefix so related tokens (same account, different expiry) share a bucket.
const (
	rateLimiterCapacity = 20
	rateLimiterRefill   = 10        // tokens per second
	rateLimiterTick     = time.Second / 2
	keyPrefixLen        = 32
)

type rateLimiter struct {
	buckets map[string]*bucket
	mu      sync.Mutex
	rate    float64
	cap     int
	ticker  *time.Ticker
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(rate float64, cap int) *rateLimiter {
	rl := &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		cap:     cap,
		ticker:  time.NewTicker(rateLimiterTick),
	}
	go rl.refillLoop()
	return rl
}

func (rl *rateLimiter) refillLoop() {
	for range rl.ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for _, b := range rl.buckets {
			elapsed := now.Sub(b.last).Seconds()
			b.tokens = minFloat(rl.rate*elapsed+b.tokens, float64(rl.cap))
			b.last = now
		}
		rl.mu.Unlock()
	}
}

func (rl *rateLimiter) allow(key string) bool {
	prefix := keyPrefix(key)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[prefix]
	now := time.Now()
	if !ok {
		b = &bucket{tokens: float64(rl.cap), last: now}
		rl.buckets[prefix] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens = minFloat(rl.rate*elapsed+b.tokens, float64(rl.cap))
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func (rl *rateLimiter) stop() {
	if rl.ticker != nil {
		rl.ticker.Stop()
	}
}

func keyPrefix(token string) string {
	token = strings.TrimSpace(token)
	if len(token) > keyPrefixLen {
		return token[:keyPrefixLen]
	}
	return token
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func initRateLimiterFromEnv() *rateLimiter {
	rateStr := getEnv("UPSTREAM_RATE_LIMIT_RPS", "10")
	var rate float64
	fmt.Sscanf(rateStr, "%f", &rate)
	if rate <= 0 {
		rate = 10
	}
	return newRateLimiter(rate, rateLimiterCapacity)
}
