package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
			writeUploadStatusRetry(w)
			return
		}
		// During the startup inventory an unknown id may simply not have been
		// adopted yet, so say so rather than declaring it lost.
		if job == nil && recovering {
			results[id] = uploadStatusEntry{State: "recovering"}
			continue
		}
		entry, err := s.publicUploadState(r.Context(), job)
		if err != nil {
			writeUploadStatusRetry(w)
			return
		}
		results[id] = entry
	}
	writeJSON(w, http.StatusOK, uploadStatusResponse{Results: results})
}

// writeUploadStatusRetry refuses the whole batch as transient. A failed read
// says nothing about the uploads, so the caller must come back rather than
// treat the batch as answered.
func writeUploadStatusRetry(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	writeError(w, http.StatusServiceUnavailable, "could not read upload state")
}

// publicUploadState maps internal queue states onto the small vocabulary the
// browser understands, without leaking queue mechanics. Core failures report
// "processing" because they retry indefinitely and will eventually publish.
// A non-nil error is a transient read failure, not a verdict about the job.
func (s *Server) publicUploadState(ctx context.Context, job *store.UploadJob) (uploadStatusEntry, error) {
	if job == nil {
		return uploadStatusEntry{State: "unknown"}, nil
	}
	switch job.Status {
	case store.JobUploading:
		if job.CancellationRequestedAt != nil {
			return uploadStatusEntry{State: "cancelled"}, nil
		}
		return uploadStatusEntry{State: "uploading"}, nil
	case store.JobPending, store.JobProcessing:
		return uploadStatusEntry{State: "processing"}, nil
	case store.JobCleanup, store.JobComplete:
		// Publication already committed; source cleanup must not delay it.
		state := "published"
		if job.ResultMediaID != "" && job.ResultMediaID != job.MediaID {
			state = "duplicate"
		}
		mediaID, err := s.visibleMediaID(ctx, job.ResultMediaID)
		if err != nil {
			return uploadStatusEntry{}, err
		}
		return uploadStatusEntry{State: state, MediaID: mediaID}, nil
	case store.JobDiscarding, store.JobDiscarded:
		switch job.TerminalReason {
		// Only these two mean the guest's own intent, or bytes that were never
		// theirs to lose, ended the upload. Telling a guest they cancelled
		// something they did not is worse than reporting it failed, so any
		// other reason — including one added after this code was written —
		// reports the failure it actually was.
		case "cancelled", "unobservable":
			return uploadStatusEntry{State: "cancelled"}, nil
		default:
			return uploadStatusEntry{State: "failed"}, nil
		}
	default:
		return uploadStatusEntry{State: "processing"}, nil
	}
}

// visibleMediaID returns the id only when the item is publicly viewable.
// Awaiting approval or trashed media must not be addressable through an
// upload receipt. Absence withholds the id; any other error is transient and
// must not be rendered as a quietly missing field.
func (s *Server) visibleMediaID(ctx context.Context, mediaID string) (string, error) {
	if mediaID == "" {
		return "", nil
	}
	if _, err := s.store.GetVisibleByID(ctx, mediaID, ""); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return mediaID, nil
}
