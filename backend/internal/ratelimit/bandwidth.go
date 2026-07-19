package ratelimit

import (
	"context"
	"io"
)

// throttleChunkSize bounds how many bytes are requested from the limiter at
// once, so throughput adjusts responsively rather than in large bursts.
const throttleChunkSize = 32 * 1024

// ThrottledReader wraps an io.Reader so that reads are paced according to a
// shared per-key bandwidth limiter, capping aggregate throughput for that
// key (e.g. a source IP) across however many concurrent uploads it has in
// flight.
type ThrottledReader struct {
	ctx     context.Context
	r       io.Reader
	limiter *KeyedLimiter
	key     string
}

// NewThrottledReader returns a reader that limits throughput of r according
// to the bandwidth limiter's per-key rate for key.
func NewThrottledReader(ctx context.Context, r io.Reader, limiter *KeyedLimiter, key string) *ThrottledReader {
	return &ThrottledReader{ctx: ctx, r: r, limiter: limiter, key: key}
}

func (t *ThrottledReader) Read(p []byte) (int, error) {
	if len(p) > throttleChunkSize {
		p = p[:throttleChunkSize]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if waitErr := t.limiter.WaitN(t.ctx, t.key, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}
