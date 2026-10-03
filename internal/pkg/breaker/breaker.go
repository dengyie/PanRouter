// Package breaker 提供按网盘粒度的熔断器:
// 连续 N 次风控/上游错误后熔断一段时间,避免高频重试加重风控导致封号。
package breaker

import (
	"sync"
	"time"
)

type Breaker struct {
	mu        sync.Mutex
	fails     int
	openUntil time.Time
}

func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Now().After(b.openUntil)
}

func (b *Breaker) Record(ok bool, maxFails int, cooldown time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.fails = 0
		return
	}
	b.fails++
	if b.fails >= maxFails {
		b.openUntil = time.Now().Add(cooldown)
		b.fails = 0
	}
}

// Opened 报告当前是否处于熔断态(供指标暴露)。
func (b *Breaker) Opened() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !time.Now().After(b.openUntil)
}

type Registry struct {
	mu       sync.Mutex
	m        map[string]*Breaker
	maxFails int
	cooldown time.Duration
}

func NewRegistry(maxFails int, cooldown time.Duration) *Registry {
	if maxFails <= 0 {
		maxFails = 5
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Minute
	}
	return &Registry{m: map[string]*Breaker{}, maxFails: maxFails, cooldown: cooldown}
}

func (r *Registry) Get(key string) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.m[key]
	if !ok {
		b = &Breaker{}
		r.m[key] = b
	}
	return b
}

func (r *Registry) MaxFails() int           { return r.maxFails }
func (r *Registry) Cooldown() time.Duration { return r.cooldown }
