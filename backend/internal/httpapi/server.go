// Package httpapi wires together the public gallery API, the admin API, and
// the internal tus reverse proxy + hook handler into one HTTP server.
package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"event-gallery/backend/internal/config"
	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/ratelimit"
	"event-gallery/backend/internal/store"
)

// Server holds all shared dependencies for the HTTP API.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	processor *media.Processor

	publicLimiter       *ratelimit.KeyedLimiter
	loginLimiter        *ratelimit.KeyedLimiter
	uploadStatusLimiter *ratelimit.KeyedLimiter
	uploadConcurrency   *ratelimit.ConcurrencyLimiter
	uploadBandwidth     *ratelimit.KeyedLimiter

	tusProxy *tusReverseProxy

	ingest *ingest.Manager

	cleanupWG sync.WaitGroup

	// spaHandler serves the built frontend, if configured. It is used as
	// the fallback for any request that doesn't match an API route, so the
	// client-side router can handle deep links.
	spaHandler http.Handler
}

// NewServer constructs a Server with all rate limiters and the tus reverse
// proxy configured from cfg.
func NewServer(cfg *config.Config, st *store.Store, proc *media.Processor, spaHandler http.Handler) (*Server, error) {
	s := &Server{
		cfg:        cfg,
		store:      st,
		processor:  proc,
		spaHandler: spaHandler,

		publicLimiter: ratelimit.NewKeyedLimiter(
			rate.Limit(float64(cfg.PublicRateLimitPerMinute)/60.0),
			cfg.PublicRateLimitBurst,
			30*time.Minute,
		),
		loginLimiter: ratelimit.NewKeyedLimiter(
			rate.Limit(5.0/60.0), // 5 attempts per minute per IP
			5,
			30*time.Minute,
		),
		// Polling has its own bucket so a tab watching its uploads cannot
		// spend the budget the gallery needs. Burst is a quarter minute's
		// worth, the same proportion the public limiter defaults to.
		uploadStatusLimiter: ratelimit.NewKeyedLimiter(
			rate.Limit(float64(cfg.UploadStatusRateLimitPerMinute)/60.0),
			max(cfg.UploadStatusRateLimitPerMinute/4, 1),
			30*time.Minute,
		),
		uploadConcurrency: ratelimit.NewConcurrencyLimiter(cfg.UploadConcurrencyPerIP),
		uploadBandwidth: ratelimit.NewKeyedLimiter(
			// Burst must be at least as large as the largest single chunk
			// read (see ratelimit.ThrottledReader), so allow a generous
			// multi-second burst on top of the sustained rate.
			rate.Limit(cfg.UploadBandwidthPerIPBytesPerSec),
			int(max64(cfg.UploadBandwidthPerIPBytesPerSec*2, 1<<20)),
			30*time.Minute,
		),
	}

	proxy, err := newTusReverseProxy(cfg.TusInternalURL, cfg.TusHookSecret, cfg.TrustedProxyCIDRs)
	if err != nil {
		return nil, err
	}
	s.tusProxy = proxy

	return s, nil
}

// SetIngest wires the durable ingest manager after construction, because the
// manager needs the processor and store that NewServer already holds.
func (s *Server) SetIngest(m *ingest.Manager) { s.ingest = m }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// StartCleanupLoops launches background goroutines that periodically evict
// idle rate-limiter state and expired admin sessions. stop should be closed
// on shutdown.
func (s *Server) StartCleanupLoops(stop <-chan struct{}) {
	s.publicLimiter.StartCleanup(10*time.Minute, stop)
	s.loginLimiter.StartCleanup(10*time.Minute, stop)
	s.uploadStatusLimiter.StartCleanup(10*time.Minute, stop)
	s.uploadBandwidth.StartCleanup(10*time.Minute, stop)

	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = s.store.DeleteExpiredSessions(context.Background())
			case <-stop:
				return
			}
		}
	}()

	s.cleanupWG.Add(1)
	go func() {
		defer s.cleanupWG.Done()
		s.runStorageCleanup(stop)
	}()
}

func (s *Server) WaitForCleanup() { s.cleanupWG.Wait() }

// Router builds the complete HTTP handler tree.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverPanic, s.requestLogger, securityHeaders)

	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	r.Route("/api", func(api chi.Router) {
		api.Handle("/tus", http.HandlerFunc(s.handleTusProxy))
		api.Handle("/tus/*", http.HandlerFunc(s.handleTusProxy))
		api.Post("/internal/tus-hooks", s.handleTusHook)

		// A sibling of the public group, not a member: chi copies the parent
		// chain into an inline group, so registering it inside pub would spend
		// the gallery budget as well as this route's own.
		api.With(s.uploadStatusRateLimit).Post("/uploads/status", s.handleUploadStatus)

		api.Group(func(pub chi.Router) {
			pub.Use(s.publicRateLimit)
			pub.Get("/gallery", s.handleGallery)
			pub.Get("/config/public", s.handlePublicConfig)
			pub.Post("/uploads/check", s.handleUploadCheck)
			pub.Get("/media/{id}/thumbnail", s.handleThumbnail)
			pub.Get("/media/{id}/file", s.handleMediaFile)
			pub.Get("/media/{id}/download", s.handleMediaDownload)
			pub.Post("/media/{id}/like", s.handleLike)
			pub.Delete("/media/{id}/like", s.handleUnlike)
		})

		api.Route("/admin", func(adm chi.Router) {
			adm.With(s.publicRateLimit).Post("/login", s.handleAdminLogin)
			adm.With(s.requireAdmin).Get("/session", s.handleAdminSession)
			adm.With(s.requireAdmin, s.requireCSRF).Post("/logout", s.handleAdminLogout)
			adm.With(s.requireAdmin).Get("/media", s.handleAdminListMedia)
			adm.With(s.requireAdmin).Get("/media/{id}/thumbnail", s.handleAdminThumbnail)
			adm.With(s.requireAdmin, s.requireCSRF).Post("/media/bulk-approve", s.handleBulkApprove)
			adm.With(s.requireAdmin, s.requireCSRF).Post("/media/bulk-delete", s.handleBulkDelete)
			adm.With(s.requireAdmin, s.requireCSRF).Post("/media/bulk-restore", s.handleBulkRestore)
			adm.With(s.requireAdmin, s.requireCSRF).Post("/media/bulk-purge", s.handleBulkPurge)
			adm.With(s.requireAdmin).Get("/audit-log", s.handleAuditLog)
			adm.With(s.requireAdmin).Get("/config", s.handleAdminGetConfig)
			adm.With(s.requireAdmin, s.requireCSRF).Put("/config", s.handleAdminUpdateConfig)
			adm.With(s.requireAdmin).Get("/branding", s.handleAdminGetBranding)
			adm.With(s.requireAdmin, s.requireCSRF).Put("/branding", s.handleAdminUpdateBranding)
			adm.With(s.requireAdmin).Get("/moderation", s.handleAdminGetModeration)
			adm.With(s.requireAdmin, s.requireCSRF).Put("/moderation", s.handleAdminUpdateModeration)
			adm.With(s.requireAdmin, s.requireCSRF).Delete("/branding", s.handleAdminResetBranding)
		})
	})

	if s.spaHandler != nil {
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			s.spaHandler.ServeHTTP(w, r)
		})
	}

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports whether this instance can accept uploads: the startup
// inventory has finished and the media volume is proven. Read-only gallery
// serving does not depend on either, so a caller that pulls the whole instance
// out of rotation on a 503 here is taking more offline than the fault costs.
// /healthz stays a shallow liveness check so the gallery and the tunnel start
// promptly.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.ingest == nil || !s.ingest.Ready() {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "ingest is still recovering queued uploads")
		return
	}
	if !s.ingest.Health().Healthy() {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusServiceUnavailable, "media storage is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
