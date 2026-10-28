package telegram_user

import (
	"fmt"
	"strconv"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func parseRateLimit(opts map[string]any) (core.RateLimitCfg, error) {
	limit, err := positiveOption(opts, "rate_limit_messages", 10)
	if err != nil {
		return core.RateLimitCfg{}, err
	}
	seconds, err := positiveOption(opts, "rate_limit_window_seconds", 60)
	if err != nil {
		return core.RateLimitCfg{}, err
	}
	return core.RateLimitCfg{MaxMessages: limit, Window: time.Duration(seconds) * time.Second}, nil
}

func positiveOption(opts map[string]any, name string, fallback int) (int, error) {
	v, ok := opts[name]
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseInt(fmt.Sprint(v), 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("telegram_user: %s must be a positive integer", name)
	}
	return int(n), nil
}

func (p *Platform) allowSender(id string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.limiter != nil && p.limiter.Allow(id)
}

// Caller holds p.mu. Serialize Stop because core's limiter owns a cleanup
// goroutine; concurrent platform shutdown must not close its channel twice.
func (p *Platform) stopLimiterLocked() {
	if p.limiter != nil {
		p.limiter.Stop()
		p.limiter = nil
	}
}
