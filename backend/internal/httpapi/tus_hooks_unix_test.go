//go:build unix

package httpapi

import (
	"testing"

	"event-gallery/backend/internal/ingest"
)

// The free space floor is only enforceable where ingest can read a
// filesystem's free bytes, which is unix; elsewhere it reports math.MaxInt64
// and no floor is ever crossed.
func TestPreCreateRefusesWhenTheFreeSpaceFloorWouldBeCrossed(t *testing.T) {
	h := newTestHarness(t)
	// A floor no filesystem can satisfy, so the refusal does not depend on the
	// test machine's free space.
	manager := ingest.New(h.store, h.proc, ingest.Options{
		UploadDir:    h.cfg.TusUploadDir,
		MinFreeBytes: 1 << 62,
	})
	manager.Start()
	t.Cleanup(manager.Stop)
	h.server.SetIngest(manager)

	resp := postHook(t, h, tusHookRequest{
		Type:  "pre-create",
		Event: tusHookEvent{Upload: tusHookUpload{Size: 10, MetaData: map[string]string{"filename": "a.jpg"}}},
	})

	assertRetryable(t, resp)
	assertNoUploadJobs(t, h)
}
