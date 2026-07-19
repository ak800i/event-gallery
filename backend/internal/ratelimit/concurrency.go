package ratelimit

import (
	"sync"
)

// ConcurrencyLimiter caps the number of simultaneous in-flight operations
// (in practice, tus PATCH upload requests) allowed per key. Unlike request
// rate limiting, this bounds parallelism rather than throughput over time.
type ConcurrencyLimiter struct {
	mu      sync.Mutex
	max     int
	current map[string]int
}

// NewConcurrencyLimiter allows at most max concurrent operations per key.
func NewConcurrencyLimiter(max int) *ConcurrencyLimiter {
	return &ConcurrencyLimiter{max: max, current: make(map[string]int)}
}

// TryAcquire attempts to reserve one slot for key. It returns a release
// function to call when the operation completes, and ok=false if the key
// is already at its concurrency limit (release will be nil in that case).
func (c *ConcurrencyLimiter) TryAcquire(key string) (release func(), ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current[key] >= c.max {
		return nil, false
	}
	c.current[key]++
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.current[key]--
		if c.current[key] <= 0 {
			delete(c.current, key)
		}
	}, true
}

// Current reports how many operations are in flight for key (mainly for
// tests/observability).
func (c *ConcurrencyLimiter) Current(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current[key]
}
