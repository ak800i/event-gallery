// Package httpapi wires together the public gallery API, the admin API, and
// the internal tus reverse proxy + hook handler into one HTTP server.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/time/rate"

	"wedding-gallery/backend/internal/config"
	"wedding-gallery/backend/internal/media"
	"wedding-gallery/backend/internal/ratelimit"
	"wedding-gallery/backend/internal/store"
)

// Server holds all shared dependencies for the HTTP API.
type Server struct {
	cfg       *config.Config
	store     *store.Store
	processor *media.Processor

	publicLimiter     *ratelimit.KeyedLimiter
	loginLimiter      *ratelimit.KeyedLimiter
	uploadConcurrency *ratelimit.ConcurrencyLimiter
	uploadBandwidth   *ratelimit.KeyedLimiter

	tusProxy *tusReverseProxy

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
}

// Router builds the complete HTTP handler tree.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverPanic, s.requestLogger, securityHeaders)

	r.Get("/healthz", s.handleHealth)

	r.Route("/api", func(api chi.Router) {
		api.Handle("/tus", http.HandlerFunc(s.handleTusProxy))
		api.Handle("/tus/*", http.HandlerFunc(s.handleTusProxy))
		api.Post("/internal/tus-hooks", s.handleTusHook)

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
			adm.With(s.requireAdmin, s.requireCSRF).Post("/media/bulk-delete", s.handleBulkDelete)
			adm.With(s.requireAdmin, s.requireCSRF).Post("/media/bulk-restore", s.handleBulkRestore)
			adm.With(s.requireAdmin).Get("/audit-log", s.handleAuditLog)
			adm.With(s.requireAdmin).Get("/config", s.handleAdminGetConfig)
			adm.With(s.requireAdmin, s.requireCSRF).Put("/config", s.handleAdminUpdateConfig)
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
