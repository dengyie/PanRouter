// Package limiter 提供每网盘独立的令牌桶限频(速率可热更新)。
package limiter

import (
	"sync"
	"time"
)

type tokenBucket struct {
	tokens float64
	last   time.Time
	rate   float64 // tokens per second
}

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	now     func() time.Time
}

func New() *Limiter { return NewWithClock(time.Now) }

func NewWithClock(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{buckets: map[string]*tokenBucket{}, now: now}
}

func capacity(rate float64) float64 {
	if rate < 1 {
		return 1
	}
	return rate
}

func refill(b *tokenBucket, now time.Time, rate float64) {
	if b.rate > 0 {
		b.tokens += now.Sub(b.last).Seconds() * b.rate
	}
	cap := capacity(rate)
	if b.tokens > cap {
		b.tokens = cap
	}
	b.last, b.rate = now, rate
}

// Allow 取一个令牌;rate<=0 表示不限流。
func (l *Limiter) Allow(key string, rate float64) bool {
	if rate <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: capacity(rate), last: now, rate: rate}
		l.buckets[key] = b
	}
	if ok {
		refill(b, now, rate)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// SetRate 热更新速率(配置热加载时调用)。
func (l *Limiter) SetRate(key string, rate float64) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		refill(b, now, rate)
	}
}
