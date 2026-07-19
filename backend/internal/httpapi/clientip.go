package httpapi

import (
	"net"
	"net/http"
	"strings"
)

// clientIP extracts the best-effort real client IP address for rate
// limiting and audit logging purposes.
//
// This deployment sits behind Cloudflare, which sets CF-Connecting-IP to
// the true client address; we trust that header first since only
// Cloudflare (or our own reverse proxy in front of it) can reach this
// service in production. We fall back to the first entry of
// X-Forwarded-For and finally to the raw socket address so the server
// still degrades gracefully in local/dev setups without Cloudflare.
func clientIP(r *http.Request) string {
	if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
		return strings.TrimSpace(cf)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
