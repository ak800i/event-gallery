package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/ratelimit"
	"event-gallery/backend/internal/store"
)

// internalProxySecretHeader carries a shared secret from this backend to
// the internal tusd instance on every proxied request. tusd is configured
// (via -hooks-http-forward-headers) to copy this header's value into every
// hook HTTP call it makes back to handleTusHook, which verifies it. Since
// tusd is not reachable from the internet at all (see docker-compose.yml
// network isolation), this is defense in depth rather than the sole
// safeguard, but it also means the hook endpoint itself cannot be invoked
// by anyone who doesn't know this secret, even if network isolation were
// ever misconfigured.
const internalProxySecretHeader = "X-Internal-Proxy-Secret"

// clientIPHeader forwards the real client IP (as seen by this backend,
// which trusts Cloudflare/CF-Connecting-IP -- see clientip.go) through to
// tusd, which in turn forwards it into hook payloads so upload processing
// can record an accurate uploader IP for the audit log.
const clientIPHeader = "X-Event-Gallery-Client-Ip"

type tusReverseProxy struct {
	proxy      *httputil.ReverseProxy
	hookSecret string
}

func newTusReverseProxy(targetURL, hookSecret string, trustedProxies []netip.Prefix) (*tusReverseProxy, error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		ip := clientIP(req, trustedProxies)
		baseDirector(req)
		req.Host = target.Host
		req.URL.Path = "/files" + strings.TrimPrefix(req.URL.Path, "/api/tus")
		req.Header.Set(internalProxySecretHeader, hookSecret)
		req.Header.Set(clientIPHeader, ip)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusBadGateway, "upload service temporarily unavailable")
	}
	// tusd returns an absolute Location header pointing at its own internal
	// address (e.g. http://tusd:1080/files/<id>) on upload creation. That URL
	// is unreachable from the guest's browser (tusd is on an internal-only
	// docker network) and would be blocked as mixed content on an HTTPS page,
	// so tus-js-client's follow-up PATCH/HEAD requests fail and no upload ever
	// completes. Rewrite the Location back to this backend's public tus route
	// so the client keeps talking to us instead of tusd directly.
	proxy.ModifyResponse = func(resp *http.Response) error {
		if loc := resp.Header.Get("Location"); loc != "" {
			if u, err := url.Parse(loc); err == nil {
				resp.Header.Set("Location", "/api/tus/"+path.Base(u.Path))
			}
		}
		return nil
	}
	return &tusReverseProxy{proxy: proxy, hookSecret: hookSecret}, nil
}

// handleTusProxy forwards tus protocol requests (POST to create, PATCH to
// send chunks, HEAD to resume) to the internal tusd instance, applying per-IP
// concurrency and bandwidth limits to the data-carrying PATCH requests and
// blocking new uploads once the admin has set an upload expiry in the past.
//
// DELETE is the exception: it is answered here and never forwarded, because
// tusd's terminate would remove a guest's only copy of their file.
func (s *Server) handleTusProxy(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.cfg.TrustedProxyCIDRs)
	if !s.publicLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded, please slow down")
		return
	}

	uploadID := tusUploadIDFromPath(r.URL.Path)

	// Unconditional, and before anything trusts the id: refusing every client
	// DELETE here is what makes terminating a durable upload from a browser
	// structurally impossible rather than merely disallowed.
	if r.Method == http.MethodDelete {
		// The fence runs first. A client that gave up under backpressure sends
		// this DELETE with all of its bytes already on disk; committing them
		// here is what turns the cancellation into a 409 instead of a deleted
		// file.
		if s.fenceCompletedUpload(w, r, uploadID) {
			return
		}
		s.handleTusDelete(w, r, uploadID)
		return
	}

	if (r.Method == http.MethodHead || r.Method == http.MethodPatch) && s.fenceCompletedUpload(w, r, uploadID) {
		return
	}

	switch r.Method {
	case http.MethodPost:
		closed, err := s.uploadsClosed(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to check upload availability")
			return
		}
		if closed {
			writeError(w, http.StatusForbidden, "uploads are closed for this gallery")
			return
		}
	case http.MethodPatch:
		release, ok := s.uploadConcurrency.TryAcquire(ip)
		if !ok {
			writeError(w, http.StatusTooManyRequests, "too many concurrent uploads in progress, please wait")
			return
		}
		defer release()
		r.Body = io.NopCloser(ratelimit.NewThrottledReader(r.Context(), r.Body, s.uploadBandwidth, ip))
	}

	s.tusProxy.proxy.ServeHTTP(w, r)
}

// fenceRetryAfterSeconds paces a client whose upload we could not commit
// inside its request budget.
const fenceRetryAfterSeconds = 5

func fenceRetry(w http.ResponseWriter, message string) {
	w.Header().Set("Retry-After", strconv.Itoa(fenceRetryAfterSeconds))
	writeError(w, http.StatusServiceUnavailable, message)
}

// fenceCompletedUpload stops the proxy from reporting success that the
// database has not recorded. tusd answers an already-complete upload with a
// plain 204 without re-running pre-finish, so without this a client that was
// told 503 could retry and be told success while the row is still 'uploading'.
// It returns true once it has written a response, meaning the request must not
// be forwarded.
//
// It fails closed: if we cannot read the row, we do not know whether success
// would be truthful, and an unnecessary retry is always cheaper than a false
// success.
func (s *Server) fenceCompletedUpload(w http.ResponseWriter, r *http.Request, uploadID string) bool {
	if s.ingest == nil {
		return false
	}
	// Before the startup inventory finishes we cannot tell a rowless orphan
	// from an upload we have not inventoried yet, so nothing is forwarded.
	if !s.ingest.Ready() {
		fenceRetry(w, "server is still recovering queued uploads")
		return true
	}
	if uploadID == "" {
		return false
	}
	job, err := s.store.GetUploadJob(r.Context(), uploadID)
	if err != nil {
		slog.Warn("completion fence could not read the upload row", "operation", "fence", "upload_id", uploadID, "error", err)
		fenceRetry(w, "upload state is temporarily unavailable, please retry")
		return true
	}
	if job == nil || job.Status != store.JobUploading {
		return false
	}

	stat, err := os.Stat(s.ingest.DataPath(uploadID))
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != job.ExpectedSize {
		return false // still uploading; let the transfer continue
	}

	// Bound the wait with the same budget the hook uses. A proxied HEAD or
	// PATCH has no deadline of its own, and the operation it joins is bounded
	// by the processing timeout, so without this the client could hang.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UploadDurabilityWait)
	defer cancel()
	switch err := s.ingest.EnsureDurable(ctx, uploadID); {
	case err == nil:
		return false // now durable; forwarding is safe
	case errors.Is(err, ingest.ErrDurabilityFinal):
		// Nothing about this upload can change, so backpressure would leave the
		// browser retrying every five seconds forever. 410 and never 409, 423
		// or 429: @uppy/tus installs its own onShouldRetry, which retries all
		// three, so none of them is terminal at the client.
		slog.Warn("completion fence refused an upload that can never commit",
			"operation", "fence", "upload_id", uploadID, "error", err)
		writeError(w, http.StatusGone, "upload can no longer be completed")
		return true
	default:
		slog.Warn("completion fence could not commit the upload in time",
			"operation", "fence", "upload_id", uploadID, "error", err)
		fenceRetry(w, "upload is still being persisted, please retry")
		return true
	}
}

// handleTusDelete consumes a public DELETE as cancellation intent. Recording
// intent is all it does: removing the source belongs to the janitor, which
// first claims the row to discarding.
func (s *Server) handleTusDelete(w http.ResponseWriter, r *http.Request, uploadID string) {
	if uploadID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	job, err := s.store.GetUploadJob(r.Context(), uploadID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read upload state")
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if job.Status != store.JobUploading {
		writeError(w, http.StatusConflict, "upload already completed and cannot be cancelled")
		return
	}
	if err := s.store.RequestCancellation(r.Context(), uploadID, store.NowMicros()); err != nil {
		if !errors.Is(err, store.ErrNotClaimed) {
			writeError(w, http.StatusInternalServerError, "could not record cancellation")
			return
		}
		// The row moved on between our read and this write — a promotion the
		// fence or the pre-finish hook committed. The answer is the same as if
		// we had seen it above.
		writeError(w, http.StatusConflict, "upload already completed and cannot be cancelled")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tusUploadIDFromPath returns the final path segment of /api/tus/<id>.
func tusUploadIDFromPath(p string) string {
	id := path.Base(strings.TrimSuffix(p, "/"))
	if id == "tus" || id == "." || id == "/" {
		return ""
	}
	if !safeUploadID(id) {
		return ""
	}
	return id
}

// Terminate implements ingest.SourceTerminator. Both the incomplete-upload
// janitor and the ingest workers remove tus files through this one path, so
// tusd always cleans up its own sidecar and lock state.
//
// It refuses to remove a source the queue may still need. An uploading row is
// refused along with pending and processing: its final PATCH can commit at any
// moment, so a caller must first claim it to discarding, which is the only
// transition that cannot lose that race.
func (s *Server) Terminate(ctx context.Context, uploadID string) error {
	job, err := s.store.GetUploadJob(ctx, uploadID)
	if err != nil {
		return err
	}
	if job != nil && (job.Status == store.JobUploading || job.Status == store.JobPending || job.Status == store.JobProcessing) {
		return fmt.Errorf("upload %s is still owned by the queue and must not be terminated", uploadID)
	}
	return s.terminateTusUpload(ctx, uploadID)
}

// uploadsClosed reports whether the admin-configured upload expiry has
// passed. Expiry only ever blocks new uploads; existing media remains
// viewable and downloadable regardless.
func (s *Server) uploadsClosed(ctx context.Context) (bool, error) {
	value, ok, err := s.store.GetConfig(ctx, store.ConfigKeyUploadExpiresAt)
	if err != nil {
		return false, err
	}
	if !ok || value == "" {
		return false, nil
	}
	expires, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		// A corrupt config value should not accidentally block all
		// uploads; log via caller and fail open (uploads stay allowed).
		return false, nil
	}
	return time.Now().After(expires), nil
}
