package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"event-gallery/backend/internal/store"
)

const maxStatusBatch = 100

type uploadStatusRequest struct {
	UploadIDs []string `json:"uploadIds"`
}

type uploadStatusEntry struct {
	State   string `json:"state"`
	MediaID string `json:"mediaId,omitempty"`
}

type uploadStatusResponse struct {
	Results map[string]uploadStatusEntry `json:"results"`
}

// handleUploadStatus answers "what happened to my uploads?" for a batch of
// upload ids. An upload id is the only credential involved, so an entry says
// how far the upload got and nothing else about it.
func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	var req uploadStatusRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.UploadIDs) == 0 || len(req.UploadIDs) > maxStatusBatch {
		writeError(w, http.StatusBadRequest, "uploadIds must contain between 1 and 100 entries")
		return
	}

	results := make(map[string]uploadStatusEntry, len(req.UploadIDs))
	recovering := s.ingest != nil && !s.ingest.Ready()
	for _, id := range req.UploadIDs {
		if _, seen := results[id]; seen {
			continue
		}
		job, err := s.store.GetUploadJob(r.Context(), id)
		if err != nil {
			// Failing to read the queue says nothing about the upload, so the
			// caller must come back rather than treat the batch as answered.
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "could not read upload state")
			return
		}
		// During the startup inventory an unknown id may simply not have been
		// adopted yet, so say so rather than declaring it lost.
		if job == nil && recovering {
			results[id] = uploadStatusEntry{State: "recovering"}
			continue
		}
		results[id] = s.publicUploadState(r.Context(), job)
	}
	writeJSON(w, http.StatusOK, uploadStatusResponse{Results: results})
}

// publicUploadState maps internal queue states onto the small vocabulary the
// browser understands, without leaking queue mechanics. Core failures report
// "processing" because they retry indefinitely and will eventually publish.
func (s *Server) publicUploadState(ctx context.Context, job *store.UploadJob) uploadStatusEntry {
	if job == nil {
		return uploadStatusEntry{State: "unknown"}
	}
	switch job.Status {
	case store.JobUploading:
		if job.CancellationRequestedAt != nil {
			return uploadStatusEntry{State: "cancelled"}
		}
		return uploadStatusEntry{State: "uploading"}
	case store.JobPending, store.JobProcessing:
		return uploadStatusEntry{State: "processing"}
	case store.JobCleanup, store.JobComplete:
		// Publication already committed; source cleanup must not delay it.
		state := "published"
		if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
			state = "duplicate"
		}
		return uploadStatusEntry{State: state, MediaID: s.visibleMediaID(ctx, job.ResultMediaID)}
	case store.JobDiscarding, store.JobDiscarded:
		switch job.TerminalReason {
		case "unsupported_type", "checksum_mismatch":
			return uploadStatusEntry{State: "failed"}
		default:
			return uploadStatusEntry{State: "cancelled"}
		}
	default:
		return uploadStatusEntry{State: "processing"}
	}
}

// visibleMediaID returns the id only when the item is publicly viewable.
// Awaiting approval or trashed media must not be addressable through an
// upload receipt.
func (s *Server) visibleMediaID(ctx context.Context, mediaID string) string {
	if mediaID == "" {
		return ""
	}
	if _, err := s.store.GetVisibleByID(ctx, mediaID, ""); err != nil {
		return ""
	}
	return mediaID
}
