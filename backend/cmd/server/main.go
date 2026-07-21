// Command server runs the wedding-gallery backend: the public gallery API,
// the admin API, and the internal reverse proxy + hook handler in front of
// tusd. It also serves the built frontend as a single-page application.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Ensures time.LoadLocation/time.Local work correctly even on minimal
	// container images that might be missing /usr/share/zoneinfo, so the
	// TZ environment variable (e.g. Europe/Belgrade) is always honored.
	_ "time/tzdata"

	"wedding-gallery/backend/internal/config"
	"wedding-gallery/backend/internal/db"
	"wedding-gallery/backend/internal/httpapi"
	"wedding-gallery/backend/internal/media"
	"wedding-gallery/backend/internal/staticui"
	"wedding-gallery/backend/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	sqlDB, err := db.Open(cfg.DataDir + "/gallery.db")
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	st := store.New(sqlDB)
	processor := media.NewProcessor(cfg.MediaDir, cfg.ThumbnailMaxDimension, cfg.AllowedImageMIMEs, cfg.AllowedVideoMIMEs)
	if err := processor.EnsureDirs(); err != nil {
		return err
	}
	backfillCtx, backfillCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := repairVideoDisplayDimensions(backfillCtx, st, processor); err != nil {
		slog.Warn("video display-dimension backfill incomplete; will retry on next start", "error", err)
	}
	backfillCancel()

	spaHandler, err := staticui.Handler()
	if err != nil {
		return err
	}

	srv, err := httpapi.NewServer(cfg, st, processor, spaHandler)
	if err != nil {
		return err
	}

	stop := make(chan struct{})
	srv.StartCleanupLoops(stop)
	defer close(stop)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 15 * time.Second,
		// No blanket ReadTimeout/WriteTimeout: large tus PATCH uploads and
		// video streaming downloads legitimately run far longer than a
		// typical JSON API request.
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", cfg.ListenAddr, "tz", cfg.Timezone)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	return httpServer.Shutdown(shutdownCtx)
}

func repairVideoDisplayDimensions(ctx context.Context, st *store.Store, processor *media.Processor) error {
	if value, ok, err := st.GetConfig(ctx, store.ConfigKeyVideoRotationBackfill); err != nil {
		return err
	} else if ok && value != "" {
		return nil
	}

	records, err := st.ListVideoMetadata(ctx)
	if err != nil {
		return err
	}
	repaired := 0
	failed := 0
	for _, record := range records {
		info, err := media.ProbeVideo(ctx, processor.OriginalPath(record.StoredFilename))
		if err != nil {
			failed++
			slog.Warn("failed to re-probe video dimensions", "media_id", record.ID, "error", err)
			continue
		}
		if info.Width <= 0 || info.Height <= 0 {
			failed++
			slog.Warn("video probe returned invalid dimensions", "media_id", record.ID)
			continue
		}
		if info.Width == record.Width && info.Height == record.Height {
			continue
		}
		if err := st.UpdateVideoDimensions(ctx, record.ID, info.Width, info.Height); err != nil {
			failed++
			slog.Warn("failed to repair video dimensions", "media_id", record.ID, "error", err)
			continue
		}
		repaired++
		slog.Info("repaired video display dimensions", "media_id", record.ID, "old_width", record.Width, "old_height", record.Height, "width", info.Width, "height", info.Height)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d video metadata records failed", failed, len(records))
	}
	if err := st.SetConfig(ctx, store.ConfigKeyVideoRotationBackfill, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	slog.Info("video display-dimension backfill complete", "videos", len(records), "repaired", repaired)
	return nil
}
