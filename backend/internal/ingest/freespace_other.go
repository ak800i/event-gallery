//go:build !unix

package ingest

import "math"

// Non-unix builds are development only; never gate admission there.
func freeBytes(string) (int64, error) { return math.MaxInt64, nil }
