// Package ratelimit provides per-source-IP request rate limiting, upload
// concurrency capping, and upload bandwidth throttling. All three exist to
// keep a single guest (malicious or just an over-eager phone) from
// degrading the experience for everyone else on a modest self-hosted box
// and home internet connection.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// KeyedLimiter maintains one token-bucket rate.Limiter per string key
// (typically a client IP address), lazily created and evicted after a
// period of inactivity so memory usage stays bounded regardless of how
// many distinct visitors have ever connected.
type KeyedLimiter struct {
	mu       sync.Mutex
	limit    rate.Limit
	burst    int
	entries  map[string]*keyedEntry
	idleTime time.Duration
}

type keyedEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewKeyedLimiter creates a limiter allowing `limit` events per second with
// the given burst, per key. idleTime controls how long an unused key's
// state is retained before Cleanup evicts it.
func NewKeyedLimiter(limit rate.Limit, burst int, idleTime time.Duration) *KeyedLimiter {
	return &KeyedLimiter{
		limit:    limit,
		burst:    burst,
		entries:  make(map[string]*keyedEntry),
		idleTime: idleTime,
	}
}

// Allow reports whether an event for key is permitted right now, consuming
// a token if so.
func (k *KeyedLimiter) Allow(key string) bool {
	return k.getLimiter(key).Allow()
}

func (k *KeyedLimiter) getLimiter(key string) *rate.Limiter {
	k.mu.Lock()
	entry, ok := k.entries[key]
	if !ok {
		entry = &keyedEntry{limiter: rate.NewLimiter(k.limit, k.burst)}
		k.entries[key] = entry
	}
	entry.lastSeen = time.Now()
	limiter := entry.limiter
	k.mu.Unlock()
	return limiter
}

// WaitN blocks until n tokens are available for key (or ctx is done),
// consuming them. It is used to throttle byte-oriented throughput (upload
// bandwidth) rather than discrete request counts.
func (k *KeyedLimiter) WaitN(ctx context.Context, key string, n int) error {
	return k.getLimiter(key).WaitN(ctx, n)
}

// Cleanup removes entries that have not been used within idleTime. Intended
// to be called periodically (see StartCleanup).
func (k *KeyedLimiter) Cleanup() {
	cutoff := time.Now().Add(-k.idleTime)
	k.mu.Lock()
	defer k.mu.Unlock()
	for key, entry := range k.entries {
		if entry.lastSeen.Before(cutoff) {
			delete(k.entries, key)
		}
	}
}

// StartCleanup launches a goroutine that calls Cleanup on the given
// interval until stop is closed.
func (k *KeyedLimiter) StartCleanup(interval time.Duration, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				k.Cleanup()
			case <-stop:
				return
			}
		}
	}()
}

// Len reports the current number of tracked keys (mainly for tests).
func (k *KeyedLimiter) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}
