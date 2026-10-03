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
}

func New() *Limiter {
	return &Limiter{buckets: map[string]*tokenBucket{}}
}

// Allow 取一个令牌;rate<=0 表示不限流。
func (l *Limiter) Allow(key string, rate float64) bool {
	if rate <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &tokenBucket{tokens: rate, last: now, rate: rate}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.rate { // 桶容量 = 1 秒的量
		b.tokens = b.rate
	}
	b.last, b.rate = now, rate
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// SetRate 热更新速率(配置热加载时调用)。
func (l *Limiter) SetRate(key string, rate float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.rate = rate
		if b.tokens > rate {
			b.tokens = rate
		}
	}
}
