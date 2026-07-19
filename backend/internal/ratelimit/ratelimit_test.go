package ratelimit

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestKeyedLimiter_AllowsUpToBurstThenBlocks(t *testing.T) {
	kl := NewKeyedLimiter(rate.Limit(1), 2, time.Minute)
	if !kl.Allow("ip1") {
		t.Error("expected first request allowed")
	}
	if !kl.Allow("ip1") {
		t.Error("expected second request allowed (within burst)")
	}
	if kl.Allow("ip1") {
		t.Error("expected third immediate request to be denied")
	}
}

func TestKeyedLimiter_KeysAreIndependent(t *testing.T) {
	kl := NewKeyedLimiter(rate.Limit(1), 1, time.Minute)
	if !kl.Allow("ip1") {
		t.Error("expected ip1 first request allowed")
	}
	if kl.Allow("ip1") {
		t.Error("expected ip1 second request denied")
	}
	if !kl.Allow("ip2") {
		t.Error("expected ip2 to have its own independent bucket")
	}
}

func TestKeyedLimiter_Cleanup(t *testing.T) {
	kl := NewKeyedLimiter(rate.Limit(10), 10, 10*time.Millisecond)
	kl.Allow("ip1")
	if kl.Len() != 1 {
		t.Fatalf("expected 1 tracked key, got %d", kl.Len())
	}
	time.Sleep(30 * time.Millisecond)
	kl.Cleanup()
	if kl.Len() != 0 {
		t.Errorf("expected stale key evicted, got %d remaining", kl.Len())
	}
}

func TestKeyedLimiter_WaitN(t *testing.T) {
	kl := NewKeyedLimiter(rate.Limit(1000), 1000, time.Minute)
	ctx := context.Background()
	if err := kl.WaitN(ctx, "ip1", 500); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConcurrencyLimiter_CapsPerKey(t *testing.T) {
	cl := NewConcurrencyLimiter(2)
	rel1, ok1 := cl.TryAcquire("ip1")
	if !ok1 {
		t.Fatal("expected first acquire to succeed")
	}
	_, ok2 := cl.TryAcquire("ip1")
	if !ok2 {
		t.Fatal("expected second acquire to succeed (at limit)")
	}
	if _, ok3 := cl.TryAcquire("ip1"); ok3 {
		t.Fatal("expected third acquire to fail (over limit)")
	}
	// A different key should not be affected.
	relOther, okOther := cl.TryAcquire("ip2")
	if !okOther {
		t.Fatal("expected independent key to acquire successfully")
	}
	rel1()
	if _, ok := cl.TryAcquire("ip1"); !ok {
		t.Fatal("expected acquire to succeed again after release")
	}
	relOther()
}

func TestThrottledReader_LimitsThroughputButPreservesContent(t *testing.T) {
	kl := NewKeyedLimiter(rate.Limit(1<<20), 1<<20, time.Minute) // effectively unthrottled for content correctness check
	data := strings.Repeat("abcdefgh", 1000)
	r := NewThrottledReader(context.Background(), strings.NewReader(data), kl, "ip1")

	buf := make([]byte, 0, len(data))
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	if string(buf) != data {
		t.Error("throttled reader must not alter content")
	}
}

func TestThrottledReader_ContextCancellation(t *testing.T) {
	kl := NewKeyedLimiter(rate.Limit(1), 10, time.Minute) // burst 10 bytes, refill 1 byte/sec
	ctx, cancel := context.WithCancel(context.Background())
	data := strings.Repeat("x", 100)
	r := NewThrottledReader(ctx, strings.NewReader(data), kl, "ip1")

	tmp := make([]byte, 10)
	// First read consumes the burst allowance immediately.
	if _, err := r.Read(tmp); err != nil {
		t.Fatalf("first read should succeed: %v", err)
	}
	cancel()
	if _, err := r.Read(tmp); err == nil {
		t.Error("expected error after context cancellation while waiting for bandwidth tokens")
	}
}
