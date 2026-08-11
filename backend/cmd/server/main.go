// Command server runs the event-gallery backend: the public gallery API,
// the admin API, and the internal reverse proxy + hook handler in front of
// tusd. It also serves the built frontend as a single-page application.
package main

import (
	"context"
	"errors"
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

	"event-gallery/backend/internal/config"
	"event-gallery/backend/internal/db"
	"event-gallery/backend/internal/httpapi"
	"event-gallery/backend/internal/ingest"
	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/staticui"
	"event-gallery/backend/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

// ingestOptions translates configuration into the manager's options. It is a
// standalone function only so the mapping can be tested: the fields repeat
// types, so a transposed pair compiles and then misbehaves silently.
func ingestOptions(cfg *config.Config, term ingest.SourceTerminator) ingest.Options {
	return ingest.Options{
		Workers:           cfg.MediaProcessingWorkers,
		DurabilityWorkers: cfg.UploadDurabilityWorkers,
		ProcessingTimeout: cfg.MediaProcessingTimeout,
		MaxBackoff:        cfg.UploadRetryMaxBackoff,
		ReconcileInterval: cfg.IngestReconcileInterval,
		JobRetention:      cfg.UploadJobRetention,
		UploadDir:         cfg.TusUploadDir,
		MinFreeBytes:      cfg.IngestMinFreeBytes,
		Terminator:        term,
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

	spaHandler, err := staticui.Handler()
	if err != nil {
		return err
	}

	srv, err := httpapi.NewServer(cfg, st, processor, spaHandler)
	if err != nil {
		return err
	}

	// Constructed here, before StartCleanupLoops, on purpose. That call runs
	// its first storage-cleanup pass immediately rather than waiting for the
	// ticker, and the pass reaches purgeMedia, which reads s.ingest -- a field
	// written below without synchronisation. The server is built first because
	// it is also the SourceTerminator.
	ingestManager := ingest.New(st, processor, ingestOptions(cfg, srv))
	srv.SetIngest(ingestManager)
	// Registered after `defer sqlDB.Close()`, so it runs before it: Stop waits
	// for in-flight durability commits, and those need the database open.
	defer ingestManager.Stop()

	stop := make(chan struct{})
	srv.StartCleanupLoops(stop)
	defer func() {
		close(stop)
		srv.WaitForCleanup()
	}()

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

	// Started only once the listener is up. The inventory fsyncs every
	// recovered source, and the first boot after this upgrade has the largest
	// backlog of them; blocking the listener would fail the container
	// healthcheck and keep the tunnel down. Upload routes answer 503 until
	// Ready() flips, which is the correct backpressure for that window.
	//
	// Called synchronously, not with `go`: Start takes its WaitGroup slot on
	// the calling goroutine, so a shutdown racing startup cannot see an empty
	// group, return, and close the database while recovery is still running.
	ingestManager.Start()

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
