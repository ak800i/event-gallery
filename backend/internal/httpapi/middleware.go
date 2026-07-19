package httpapi

import (
	"log/slog"
	"net/http"
	"time"
)

// publicRateLimit rejects requests once a source IP exceeds the configured
// public API rate limit. Applied to all public (non-admin, non-tus) JSON
// endpoints; the tus proxy has its own, separate concurrency/bandwidth
// controls better suited to large binary uploads.
func (s *Server) publicRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.cfg.TrustedProxyCIDRs)
		if !s.publicLimiter.Allow(ip) {
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded, please slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets a conservative baseline of security-related response
// headers appropriate for a same-origin SPA + JSON API.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(status int) {
	rec.status = status
	rec.ResponseWriter.WriteHeader(status)
}

// requestLogger logs one structured line per request with method, path,
// status, duration, and client IP -- useful for spotting abuse patterns
// even without a dedicated log aggregator.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("http_request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r, s.cfg.TrustedProxyCIDRs),
		)
	})
}

// recoverPanic converts panics in handlers into a 500 response instead of
// crashing the whole server process.
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered", "error", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
