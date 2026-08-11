package httpapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/models"
)

func TestReadyzReportsReadyOnceIngestHasRecovered(t *testing.T) {
	h := newTestHarness(t)

	rec := doRequest(h, http.MethodGet, "/readyz", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// /healthz stays a shallow liveness check so the gallery and the tunnel come
// up promptly; only readiness waits for the ingest inventory.
func TestHealthzDoesNotWaitForIngestRecovery(t *testing.T) {
	h := newTestHarness(t)
	h.server.SetIngest(ingest.New(h.store, h.proc, ingest.Options{UploadDir: h.cfg.TusUploadDir}))

	if rec := doRequest(h, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", rec.Code)
	}
	rec := doRequest(h, http.MethodGet, "/readyz", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("a recovering ingest must tell the probe when to come back")
	}
}

// A mounted-but-empty media volume is the failure the health gate exists for,
// and readiness has to surface it: a ready instance is one that can accept an
// upload, and uploads are refused while the circuit is open.
func TestReadyzRefusesWhenTheMediaVolumeCannotBeProven(t *testing.T) {
	h := newTestHarness(t)

	// A committed row whose original is not on disk: exactly what a failed
	// bind mount looks like.
	if err := h.store.InsertMedia(context.Background(), &models.MediaItem{
		ID:               "m1",
		OriginalFilename: "photo.jpg",
		StoredFilename:   "m1.jpg",
		Kind:             models.KindImage,
		MimeType:         "image/jpeg",
		SizeBytes:        1,
		SHA256:           "aa",
		UploadedAt:       time.Now(),
	}); err != nil {
		t.Fatalf("insert media: %v", err)
	}
	if err := h.ingest.Health().Check(context.Background()); err == nil {
		t.Fatal("precondition: the health gate should have opened the circuit")
	}

	rec := doRequest(h, http.MethodGet, "/readyz", nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !h.ingest.Ready() {
		t.Error("this must be the storage branch, not the recovery branch: ingest is ready")
	}
}
