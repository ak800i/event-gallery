package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"wedding-gallery/backend/internal/ratelimit"
	"wedding-gallery/backend/internal/store"
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
const clientIPHeader = "X-Wg-Client-Ip"

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

// handleTusProxy forwards all tus protocol requests (POST to create, PATCH
// to send chunks, HEAD to resume, DELETE to abort) to the internal tusd
// instance, applying per-IP concurrency and bandwidth limits to the
// data-carrying PATCH requests and blocking new uploads once the admin has
// set an upload expiry in the past.
func (s *Server) handleTusProxy(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.cfg.TrustedProxyCIDRs)
	if !s.publicLimiter.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded, please slow down")
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
