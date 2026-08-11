package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"event-gallery/backend/internal/ingest"
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

// clientStatusHook answers the hook itself with 2xx — the only way tusd v2.10
// relays a chosen status, since a non-2xx body becomes an opaque 500 at the
// browser — and embeds the response the client must see. reject asks tusd to
// abort the upload, which it honours for pre-create only. A non-positive
// retryAfterSeconds omits the header, which is what separates a final refusal
// from backpressure.
func clientStatusHook(w http.ResponseWriter, reject bool, status int, message string, retryAfterSeconds int) {
	header := map[string]string{"Content-Type": "application/json"}
	if retryAfterSeconds > 0 {
		header["Retry-After"] = strconv.Itoa(retryAfterSeconds)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tusHookResponse{
		RejectUpload: reject,
		HTTPResponse: &tusHookHTTPResponse{
			StatusCode: status,
			Body:       `{"error":"` + message + `"}`,
			Header:     header,
		},
	})
}

func rejectHook(w http.ResponseWriter, status int, message string) {
	clientStatusHook(w, true, status, message, 0)
}

func allowHook(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(tusHookResponse{})
}

// retryHook relays backpressure at admission time, where RejectUpload still
// means something: tusd honors it for pre-create only.
func retryHook(w http.ResponseWriter, retryAfterSeconds int, message string) {
	clientStatusHook(w, true, http.StatusServiceUnavailable, message, retryAfterSeconds)
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

// handlePreFinishHook runs inside the upload's final PATCH, and its response
// becomes that PATCH's response. Completing it successfully is precisely what
// makes the browser's success message truthful.
//
// The budget was already established by handleTusHook, so every phase here
// shares one absolute deadline.
func (s *Server) handlePreFinishHook(w http.ResponseWriter, r *http.Request, req tusHookRequest) {
	ctx := r.Context()

	upload := req.Event.Upload
	// Every refusal is logged: behind a generic client status, the server log
	// is the only trace a traversing id or a misconfigured backend leaves.
	retry := func(reason string) {
		slog.Warn("pre-finish cannot commit the upload yet",
			"operation", "pre_finish", "upload_id", upload.ID, "reason", reason)
		preFinishRetry(w, reason)
	}
	final := func(reason string) {
		slog.Warn("pre-finish refused an upload it can never commit",
			"operation", "pre_finish", "upload_id", upload.ID, "reason", reason)
		preFinishFinal(w, http.StatusBadRequest, reason)
	}

	if s.ingest == nil {
		retry("ingest is not ready")
		return
	}
	if !safeUploadID(upload.ID) {
		final("invalid upload id")
		return
	}
	if upload.Storage.Type != "filestore" {
		final("unsupported storage backend")
		return
	}
	// The hook must be talking about the path we derive, not one it chose.
	if filepath.Clean(upload.Storage.Path) != filepath.Clean(s.ingest.DataPath(upload.ID)) {
		final("unexpected storage path")
		return
	}
	if upload.Size <= 0 || upload.Offset != upload.Size {
		final("upload is not complete")
		return
	}

	switch err := s.ingest.EnsureDurable(ctx, upload.ID); {
	case err == nil:
		allowHook(w)
	case errors.Is(err, ingest.ErrDurabilityFinal):
		// The row has left the lifecycle, or the source is not the file we
		// admitted. Relaying this as backpressure would make the client retry
		// an upload that can never complete, on a five-second cadence, forever.
		slog.Warn("durability barrier refused the upload for good",
			"operation", "pre_finish", "upload_id", upload.ID, "error", err)
		preFinishFinal(w, http.StatusGone, "upload can no longer be completed")
	default:
		// Saturation, shutdown or an expired budget is backpressure. The
		// detached operation continues, so the retry will usually find it
		// already done.
		slog.Warn("durability barrier did not complete within the request budget",
			"operation", "pre_finish", "upload_id", upload.ID, "error", err)
		preFinishRetry(w, "upload is still being persisted, please retry")
	}
}

// preFinishRetryAfterSeconds paces a client whose upload we could not commit
// inside its request budget.
const preFinishRetryAfterSeconds = 5

// preFinishRetry reports a transient refusal. tusd ignores RejectUpload for
// this hook — the upload is already written and cannot be rejected here — and
// merges HTTPResponse into the final PATCH response, so the embedded 503 is
// what makes the browser back off and come back. Nothing is lost meanwhile:
// an upload we could not promote in time is picked up by reconciliation.
func preFinishRetry(w http.ResponseWriter, message string) {
	clientStatusHook(w, false, http.StatusServiceUnavailable, message, preFinishRetryAfterSeconds)
}

// preFinishFinal reports a refusal no retry can change, and is the terminal
// state the single-503 design lacked: without it a permanently truncated
// source, or a row reconciliation has discarded, leaves the client retrying
// on a five-second cadence with nothing that could ever tell it to stop. It
// carries no Retry-After, and no RejectUpload, which tusd honours at
// pre-create only. status must never be 409, 423, or 429: @uppy/tus installs
// its own onShouldRetry and never falls through to tus-js-client's, and that
// predicate retries those three, so they are not terminal at the browser.
func preFinishFinal(w http.ResponseWriter, status int, message string) {
	clientStatusHook(w, false, status, message, 0)
}

// safeUploadID rejects anything that could escape the upload directory. It is
// the ingest reconciler's own predicate, deliberately, and not a copy: the
// reconciler adopts any id it accepts, while the completion fence protects
// only ids this one accepts, so the moment the two alphabets differ the fence
// disables itself for every id in the gap. Do not reintroduce a second rule
// here.
func safeUploadID(id string) bool {
	return ingest.SafeUploadID(id)
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
