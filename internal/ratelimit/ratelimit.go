// Package ratelimit is a per-key token bucket held in memory.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

type Limiter struct {
	mu          sync.Mutex
	m           map[string]*bucket
	rate, burst float64 // tokens per second, bucket size
}

// maxKeys bounds memory. ponytail: when full, unseen keys are refused until GC
// frees room; a shared (Redis) limiter is the upgrade if that ever bites.
const maxKeys = 100_000

func New(perMinute, burst int) *Limiter {
	return &Limiter{m: map[string]*bucket{}, rate: float64(perMinute) / 60, burst: float64(burst)}
}

func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.m[key]
	if !ok {
		if len(l.m) >= maxKeys {
			return false
		}
		b = &bucket{tokens: l.burst, last: now}
		l.m[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// GC drops idle keys until ctx is done.
func (l *Limiter) GC(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.mu.Lock()
			for k, b := range l.m {
				if time.Since(b.last) > 10*time.Minute {
					delete(l.m, k)
				}
			}
			l.mu.Unlock()
		}
	}
}
