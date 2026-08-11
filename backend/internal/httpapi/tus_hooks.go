package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"event-gallery/backend/internal/store"
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

type tusHookChangeFileInfo struct {
	ID string `json:"ID,omitempty"`
}

type tusHookResponse struct {
	HTTPResponse   *tusHookHTTPResponse   `json:"HTTPResponse,omitempty"`
	RejectUpload   bool                   `json:"RejectUpload,omitempty"`
	ChangeFileInfo *tusHookChangeFileInfo `json:"ChangeFileInfo,omitempty"`
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

// retryHook relays backpressure at admission time. tusd honors RejectUpload
// only for pre-create, and only when the hook itself answers 2xx and embeds
// the real response — a non-2xx here would become an opaque 500 at the browser
// instead of a retryable 503.
func retryHook(w http.ResponseWriter, retryAfterSeconds int, message string) {
	resp := tusHookResponse{
		RejectUpload: true,
		HTTPResponse: &tusHookHTTPResponse{
			StatusCode: http.StatusServiceUnavailable,
			Body:       `{"error":"` + message + `"}`,
			Header: map[string]string{
				"Content-Type": "application/json",
				"Retry-After":  strconv.Itoa(retryAfterSeconds),
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleTusHook is the single endpoint tusd calls for every hook event
// (pre-create, post-finish, etc., configured via -hooks-http). It is only
// ever meant to be reachable from tusd itself: tusd is on an internal-only
// docker network with no published ports, and every call it makes here
// must additionally carry the shared secret this backend attached to the
// original client request (see tus_proxy.go), which tusd copies through
// via -hooks-http-forward-headers.
func (s *Server) handleTusHook(w http.ResponseWriter, r *http.Request) {
	// One absolute deadline for the whole hook, recorded before authentication
	// and body decode so no later phase can reset it. Config rejects a
	// non-positive budget, so this is always a real bound.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UploadDurabilityWait)
	defer cancel()
	r = r.WithContext(ctx)

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
		s.handlePreCreateHook(w, r, req)
	case "pre-finish":
		s.handlePreFinishHook(w, r, req)
	case "post-finish":
		// Non-blocking and unordered: treat it only as an idempotent nudge.
		if s.ingest != nil {
			s.ingest.Wake()
		}
		allowHook(w)
	default:
		allowHook(w)
	}
}

func (s *Server) handlePreCreateHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	upload := req.Event.Upload

	// Deterministic client errors keep their existing 4xx semantics. A
	// non-positive length is one of them: upload_jobs.expected_size carries a
	// CHECK (expected_size > 0), so admitting a deferred or negative size would
	// surface a constraint violation to the guest as an opaque failure.
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

	// Capacity and readiness are backpressure, not client errors.
	if s.ingest == nil || !s.ingest.Ready() {
		retryHook(w, 5, "server is still recovering queued uploads")
		return
	}
	if !s.ingest.Health().Healthy() {
		retryHook(w, 30, "media storage is unavailable")
		return
	}
	if err := s.ingest.AdmitCapacity(r.Context(), upload.Size); err != nil {
		retryHook(w, 30, "insufficient free space")
		return
	}

	uploadID, err := newUploadIdentifier()
	if err != nil {
		retryHook(w, 5, "could not allocate an upload id")
		return
	}
	if s.ingest.UploadPathsExist(uploadID) {
		retryHook(w, 1, "upload id collision, please retry")
		return
	}

	job := &store.UploadJob{
		UploadID:         uploadID,
		MediaID:          uuid.NewString(),
		OriginalFilename: sanitizeFilename(filename),
		ExpectedSize:     upload.Size,
		DeclaredSHA256:   strings.ToLower(strings.TrimSpace(upload.MetaData["sha256"])),
		GuestName:        sanitizeGuestName(upload.MetaData["guestName"], s.cfg.GuestNameMaxLength),
		UploaderIP:       hookUploaderIP(req),
	}
	if err := s.store.CreateUploadingJob(r.Context(), job); err != nil {
		slog.Error("failed to record upload job", "operation", "pre_create", "error", err)
		retryHook(w, 5, "could not record the upload")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tusHookResponse{
		ChangeFileInfo: &tusHookChangeFileInfo{ID: uploadID},
	})
}

// TODO(task-9): the durability barrier replaces this stub.
func (s *Server) handlePreFinishHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	allowHook(w)
}

// newUploadIdentifier returns a URL-safe random id. It is always freshly
// generated so no pre-create outcome can ever ask tusd to open an existing
// data or sidecar path.
func newUploadIdentifier() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hookUploaderIP(req tusHookRequest) string {
	if ip := firstHeaderValue(req.Event.HTTPRequest.Header, clientIPHeader); ip != "" {
		return ip
	}
	return req.Event.HTTPRequest.RemoteAddr
}

func firstHeaderValue(headers map[string][]string, key string) string {
	for k, values := range headers {
		if strings.EqualFold(k, key) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
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
