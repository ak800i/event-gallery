package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
)

// The following types mirror the JSON schema tusd sends for HTTP hooks, as
// documented at https://tus.github.io/tusd/advanced-topics/hooks/. We only
// decode the fields we actually use.

type tusHookStorage struct {
	Type string `json:"Type"`
	Path string `json:"Path"`
}

type tusHookUpload struct {
	ID       string            `json:"ID"`
	Size     int64             `json:"Size"`
	Offset   int64             `json:"Offset"`
	MetaData map[string]string `json:"MetaData"`
	Storage  tusHookStorage    `json:"Storage"`
}

type tusHookHTTPRequest struct {
	Method     string              `json:"Method"`
	RemoteAddr string              `json:"RemoteAddr"`
	Header     map[string][]string `json:"Header"`
}

type tusHookEvent struct {
	Upload      tusHookUpload      `json:"Upload"`
	HTTPRequest tusHookHTTPRequest `json:"HTTPRequest"`
}

type tusHookRequest struct {
	Type  string       `json:"Type"`
	Event tusHookEvent `json:"Event"`
}

type tusHookHTTPResponse struct {
	StatusCode int               `json:"StatusCode,omitempty"`
	Body       string            `json:"Body,omitempty"`
	Header     map[string]string `json:"Header,omitempty"`
}

type tusHookResponse struct {
	HTTPResponse *tusHookHTTPResponse `json:"HTTPResponse,omitempty"`
	RejectUpload bool                 `json:"RejectUpload,omitempty"`
}

func rejectHook(w http.ResponseWriter, status int, message string) {
	resp := tusHookResponse{
		RejectUpload: true,
		HTTPResponse: &tusHookHTTPResponse{
			StatusCode: status,
			Body:       `{"error":"` + message + `"}`,
			Header:     map[string]string{"Content-Type": "application/json"},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // 2XX so tusd applies our RejectUpload instruction.
	_ = json.NewEncoder(w).Encode(resp)
}

func allowHook(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tusHookResponse{})
}

// handleTusHook is the single endpoint tusd calls for every hook event
// (pre-create, post-finish, etc., configured via -hooks-http). It is only
// ever meant to be reachable from tusd itself: tusd is on an internal-only
// docker network with no published ports, and every call it makes here
// must additionally carry the shared secret this backend attached to the
// original client request (see tus_proxy.go), which tusd copies through
// via -hooks-http-forward-headers.
func (s *Server) handleTusHook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(internalProxySecretHeader) != s.cfg.TusHookSecret || s.cfg.TusHookSecret == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized hook caller")
		return
	}

	var req tusHookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid hook payload")
		return
	}

	switch req.Type {
	case "pre-create":
		s.handlePreCreateHook(w, req)
	case "post-finish":
		s.handlePostFinishHook(w, r.Context(), req)
	default:
		allowHook(w)
	}
}

func (s *Server) handlePreCreateHook(w http.ResponseWriter, req tusHookRequest) {
	upload := req.Event.Upload
	if upload.Size <= 0 {
		rejectHook(w, http.StatusBadRequest, "upload size must be known and positive")
		return
	}
	if upload.Size > s.cfg.MaxUploadBytes {
		rejectHook(w, http.StatusRequestEntityTooLarge, "file exceeds the maximum allowed size")
		return
	}
	filename := strings.TrimSpace(upload.MetaData["filename"])
	if filename == "" {
		rejectHook(w, http.StatusBadRequest, "filename metadata is required")
		return
	}
	allowHook(w)
}

func (s *Server) handlePostFinishHook(w http.ResponseWriter, ctx context.Context, req tusHookRequest) {
	upload := req.Event.Upload
	storagePath := upload.Storage.Path
	if storagePath == "" {
		slog.Error("post-finish hook missing storage path", "upload_id", upload.ID)
		allowHook(w)
		return
	}

	originalFilename := sanitizeFilename(upload.MetaData["filename"])
	guestName := sanitizeGuestName(upload.MetaData["guestName"], s.cfg.GuestNameMaxLength)
	declaredSHA256 := strings.ToLower(strings.TrimSpace(upload.MetaData["sha256"]))
	uploaderIP := firstHeaderValue(req.Event.HTTPRequest.Header, clientIPHeader)
	if uploaderIP == "" {
		uploaderIP = req.Event.HTTPRequest.RemoteAddr
	}

	defer cleanupTusInfoFile(storagePath)

	if _, err := os.Stat(storagePath); errors.Is(err, os.ErrNotExist) {
		// Already processed (e.g. a hook retry after we'd moved the file
		// away on a previous, successful attempt). Nothing to do.
		allowHook(w)
		return
	}

	result, err := s.processor.Process(ctx, storagePath, originalFilename)
	if err != nil {
		var unsupported *media.ErrUnsupportedType
		if errors.As(err, &unsupported) {
			slog.Warn("rejected upload with unsupported content", "filename", originalFilename, "guest", guestName)
			_ = s.store.RecordAudit(ctx, models.ActionUpload, actorLabel(guestName), "", originalFilename, "rejected: unsupported or unrecognized file type")
			_ = os.Remove(storagePath)
			allowHook(w)
			return
		}
		slog.Error("failed to process upload", "error", err, "filename", originalFilename)
		writeError(w, http.StatusInternalServerError, "processing failed")
		return
	}

	if declaredSHA256 != "" && declaredSHA256 != result.SHA256 {
		slog.Warn("upload sha256 mismatch, discarding", "filename", originalFilename, "declared", declaredSHA256, "actual", result.SHA256)
		_ = s.store.RecordAudit(ctx, models.ActionUpload, actorLabel(guestName), "", originalFilename, "rejected: checksum mismatch after upload")
		_ = s.processor.RemoveMedia(result.StoredFilename, result.ID)
		allowHook(w)
		return
	}

	if existing, err := s.store.GetBySHA256(ctx, result.SHA256); err == nil && existing != nil {
		slog.Info("duplicate upload ignored", "filename", originalFilename, "existing_id", existing.ID)
		_ = s.store.RecordAudit(ctx, models.ActionUpload, actorLabel(guestName), existing.ID, originalFilename, "duplicate content ignored (already in gallery)")
		_ = s.processor.RemoveMedia(result.StoredFilename, result.ID)
		allowHook(w)
		return
	}

	item := &models.MediaItem{
		ID:               result.ID,
		OriginalFilename: originalFilename,
		StoredFilename:   result.StoredFilename,
		Kind:             result.Kind,
		MimeType:         result.MimeType,
		SizeBytes:        result.SizeBytes,
		SHA256:           result.SHA256,
		Width:            result.Width,
		Height:           result.Height,
		DurationSeconds:  result.DurationSeconds,
		HasThumbnail:     result.HasThumbnail,
		CapturedAt:       result.CapturedAt,
		UploadedAt:       time.Now(),
		UploaderName:     guestName,
		UploaderIP:       uploaderIP,
	}
	if err := s.store.InsertMedia(ctx, item); err != nil {
		slog.Error("failed to insert media item", "error", err)
		_ = s.processor.RemoveMedia(result.StoredFilename, result.ID)
		writeError(w, http.StatusInternalServerError, "failed to save media item")
		return
	}
	_ = s.store.RecordAudit(ctx, models.ActionUpload, actorLabel(guestName), item.ID, originalFilename, "")

	allowHook(w)
}

func actorLabel(guestName string) string {
	if guestName == "" {
		return "anonymous guest"
	}
	return guestName
}

func firstHeaderValue(headers map[string][]string, key string) string {
	for k, values := range headers {
		if strings.EqualFold(k, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

// cleanupTusInfoFile removes tusd's sidecar `.info` metadata file after we
// have moved the actual upload data out of tusd's storage directory. Both
// containers share this volume, so the path is valid from here too.
func cleanupTusInfoFile(dataPath string) {
	infoPath := dataPath + ".info"
	if err := os.Remove(infoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("failed to remove tusd info file", "path", infoPath, "error", err)
	}
}

func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = path.Base(name) // strip any directory components
	if name == "" || name == "." || name == "/" {
		return "upload"
	}
	const maxLen = 200
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	return name
}

func sanitizeGuestName(name string, maxLen int) string {
	name = strings.TrimSpace(name)
	// Strip control characters that could otherwise corrupt log lines or
	// break rendering.
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	name = strings.TrimSpace(b.String())
	if maxLen > 0 && len(name) > maxLen {
		name = name[:maxLen]
	}
	return name
}
