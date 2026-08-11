package config

import (
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"LISTEN_ADDR", "ADMIN_PASSWORD", "SESSION_SECRET", "DATA_DIR", "MEDIA_DIR",
		"TUS_INTERNAL_URL", "TUS_HOOK_SECRET", "TUS_UPLOAD_DIR", "MAX_UPLOAD_BYTES",
		"PUBLIC_RATE_LIMIT_PER_MINUTE", "PUBLIC_RATE_LIMIT_BURST",
		"UPLOAD_CONCURRENCY_PER_IP", "UPLOAD_BANDWIDTH_PER_IP_BYTES_PER_SEC",
		"COOKIE_SECURE", "ADMIN_SESSION_TTL_MINUTES", "THUMBNAIL_MAX_DIMENSION",
		"ALLOWED_IMAGE_MIME_TYPES", "ALLOWED_VIDEO_MIME_TYPES", "GUEST_NAME_MAX_LENGTH", "TZ",
		"TRUSTED_PROXY_CIDRS", "TRASH_RETENTION_DAYS", "TUS_INCOMPLETE_RETENTION_HOURS",
		"STORAGE_CLEANUP_INTERVAL_MINUTES",
		"MEDIA_PROCESSING_WORKERS", "MEDIA_PROCESSING_TIMEOUT_MINUTES",
		"UPLOAD_DURABILITY_WAIT_SECONDS", "UPLOAD_DURABILITY_WORKERS",
		"UPLOAD_RETRY_MAX_BACKOFF_MINUTES", "INGEST_RECONCILE_INTERVAL_SECONDS",
		"INGEST_MIN_FREE_BYTES", "UPLOAD_JOB_RETENTION_DAYS",
		"UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		_ = k
	}
}

func TestLoad_MissingAdminPassword(t *testing.T) {
	clearEnv(t)
	if _, err := Load(); err == nil {
		t.Fatal("expected error when ADMIN_PASSWORD is missing")
	}
}

func TestLoad_ShortAdminPassword(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "short")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for short ADMIN_PASSWORD")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("expected default listen addr, got %s", cfg.ListenAddr)
	}
	if cfg.MaxUploadBytes != 5*1024*1024*1024 {
		t.Errorf("expected 5 GiB default max upload bytes, got %d", cfg.MaxUploadBytes)
	}
	if cfg.PublicRateLimitPerMinute != 12000 || cfg.PublicRateLimitBurst != 3000 {
		t.Errorf("unexpected public rate limit defaults: %d/minute, burst %d", cfg.PublicRateLimitPerMinute, cfg.PublicRateLimitBurst)
	}
	if cfg.UploadConcurrencyPerIP != 50 {
		t.Errorf("expected upload concurrency default 50, got %d", cfg.UploadConcurrencyPerIP)
	}
	if cfg.UploadBandwidthPerIPBytesPerSec != 1024*1024*1024 {
		t.Errorf("expected 1 GiB/s default upload bandwidth, got %d", cfg.UploadBandwidthPerIPBytesPerSec)
	}
	if len(cfg.AllowedImageMIMEs) == 0 {
		t.Errorf("expected default allowed image mime types")
	}
	if len(cfg.AllowedVideoMIMEs) == 0 {
		t.Errorf("expected default allowed video mime types")
	}
	if !cfg.CookieSecure {
		t.Errorf("expected CookieSecure to default true")
	}
	if cfg.TrashRetention != 30*24*time.Hour {
		t.Errorf("expected 30-day trash retention, got %s", cfg.TrashRetention)
	}
	if cfg.TusIncompleteRetention != 48*time.Hour {
		t.Errorf("expected 48-hour tus retention, got %s", cfg.TusIncompleteRetention)
	}
	if cfg.StorageCleanupInterval != time.Hour {
		t.Errorf("expected hourly cleanup, got %s", cfg.StorageCleanupInterval)
	}
}

func TestLoad_AllowsAVIFByDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, m := range cfg.AllowedImageMIMEs {
		if m == "image/avif" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected image/avif in default allowlist, got %v", cfg.AllowedImageMIMEs)
	}
}

func TestLoad_CustomOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("MAX_UPLOAD_BYTES", "1048576")
	t.Setenv("ALLOWED_IMAGE_MIME_TYPES", "image/jpeg, image/png")
	t.Setenv("COOKIE_SECURE", "false")
	t.Setenv("UPLOAD_CONCURRENCY_PER_IP", "5")
	t.Setenv("TRUSTED_PROXY_CIDRS", "172.30.0.0/24, 2001:db8::/32")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxUploadBytes != 1048576 {
		t.Errorf("expected overridden max upload bytes, got %d", cfg.MaxUploadBytes)
	}
	if len(cfg.AllowedImageMIMEs) != 2 || cfg.AllowedImageMIMEs[0] != "image/jpeg" {
		t.Errorf("expected overridden allowed image mime types, got %v", cfg.AllowedImageMIMEs)
	}
	if cfg.CookieSecure {
		t.Errorf("expected CookieSecure false")
	}
	if cfg.UploadConcurrencyPerIP != 5 {
		t.Errorf("expected overridden upload concurrency, got %d", cfg.UploadConcurrencyPerIP)
	}
	if len(cfg.TrustedProxyCIDRs) != 2 {
		t.Errorf("expected two trusted proxy CIDRs, got %v", cfg.TrustedProxyCIDRs)
	}
}

func TestLoad_CleanupOverridesAndDisable(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("TUS_UPLOAD_DIR", "/tmp/custom-tus")
	t.Setenv("TRASH_RETENTION_DAYS", "0")
	t.Setenv("TUS_INCOMPLETE_RETENTION_HOURS", "0")
	t.Setenv("STORAGE_CLEANUP_INTERVAL_MINUTES", "15")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrashRetention != 0 || cfg.TusIncompleteRetention != 0 || cfg.StorageCleanupInterval != 15*time.Minute || cfg.TusUploadDir != "/tmp/custom-tus" {
		t.Fatalf("unexpected cleanup config: %+v", cfg)
	}
}

func TestLoad_RejectsNegativeRetention(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("TRASH_RETENTION_DAYS", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("expected negative retention error")
	}
}

func TestLoad_InvalidInteger(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("MAX_UPLOAD_BYTES", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid integer env var")
	}
}

func TestValidate_NoAllowedMimeTypes(t *testing.T) {
	cfg := &Config{
		AdminPassword:          "supersecretpassword",
		TusHookSecret:          "supersecrethookvalue",
		MaxUploadBytes:         1,
		UploadConcurrencyPerIP: 1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when no mime types allowed")
	}
}

func TestLoad_InvalidTrustedProxyCIDR(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("TRUSTED_PROXY_CIDRS", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid trusted proxy CIDR")
	}
}

func TestValidate_MissingTusHookSecret(t *testing.T) {
	cfg := &Config{
		AdminPassword:          "supersecretpassword",
		MaxUploadBytes:         1,
		UploadConcurrencyPerIP: 1,
		AllowedImageMIMEs:      []string{"image/jpeg"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when TUS_HOOK_SECRET is missing")
	}
}

func TestLoad_IngestDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("MAX_UPLOAD_BYTES", "1000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MediaProcessingWorkers != 2 {
		t.Errorf("MediaProcessingWorkers = %d, want 2", cfg.MediaProcessingWorkers)
	}
	if cfg.UploadDurabilityWait != 75*time.Second {
		t.Errorf("UploadDurabilityWait = %v, want 75s", cfg.UploadDurabilityWait)
	}
	// Default floor is twice the largest single upload: the incoming copy
	// plus the permanent copy.
	if cfg.IngestMinFreeBytes != 2000 {
		t.Errorf("IngestMinFreeBytes = %d, want 2000", cfg.IngestMinFreeBytes)
	}
	if cfg.UploadJobRetention != 30*24*time.Hour {
		t.Errorf("UploadJobRetention = %v, want 720h", cfg.UploadJobRetention)
	}
}

func TestLoad_IngestOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("MEDIA_PROCESSING_WORKERS", "6")
	t.Setenv("INGEST_MIN_FREE_BYTES", "12345")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MediaProcessingWorkers != 6 {
		t.Errorf("MediaProcessingWorkers = %d, want 6", cfg.MediaProcessingWorkers)
	}
	if cfg.IngestMinFreeBytes != 12345 {
		t.Errorf("IngestMinFreeBytes = %d, want 12345", cfg.IngestMinFreeBytes)
	}
}

func TestLoad_RejectsDurabilityWaitAboveHookTimeout(t *testing.T) {
	clearEnv(t)
	t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
	t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
	t.Setenv("UPLOAD_DURABILITY_WAIT_SECONDS", "120")

	// 75s < 90s < 100s is load-bearing: a budget above the hook timeout means
	// tusd cuts the request before we can relay a retryable 503.
	if _, err := Load(); err == nil {
		t.Fatal("expected a budget above the 90s hook timeout to be rejected")
	}
}

func TestLoad_RejectsNonPositiveDurabilityWait(t *testing.T) {
	for _, value := range []string{"0", "-5"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
			t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
			t.Setenv("UPLOAD_DURABILITY_WAIT_SECONDS", value)

			// A non-positive budget is not "no bound": the hook then runs under
			// tusd's own 90s timeout, which severs the request before a 503 can
			// be relayed — exactly what the upper bound above exists to prevent.
			if _, err := Load(); err == nil {
				t.Fatal("expected a non-positive durability budget to be rejected")
			}
		})
	}
}

func TestLoad_RejectsNonPositiveUploadStatusRateLimit(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		t.Run(value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
			t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
			t.Setenv("UPLOAD_STATUS_RATE_LIMIT_PER_MINUTE", value)

			// A non-positive limit is not "unlimited": the status route
			// degrades to rate 0 with a burst of one, which is a single poll
			// per IP forever, with nothing in the logs to explain it.
			if _, err := Load(); err == nil {
				t.Fatal("expected a non-positive status poll limit to be rejected")
			}
		})
	}
}

func TestLoad_RejectsNonPositivePublicRateLimit(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{"zero per-minute", "PUBLIC_RATE_LIMIT_PER_MINUTE", "0"},
		{"negative per-minute", "PUBLIC_RATE_LIMIT_PER_MINUTE", "-1"},
		{"zero burst", "PUBLIC_RATE_LIMIT_BURST", "0"},
		{"negative burst", "PUBLIC_RATE_LIMIT_BURST", "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ADMIN_PASSWORD", "supersecretpassword")
			t.Setenv("TUS_HOOK_SECRET", "supersecrethookvalue")
			t.Setenv(tc.key, tc.value)

			// rate.Limit(0) allows no events, so this is not "unlimited" -- it
			// spends the burst and then blocks that IP forever. The gallery
			// stops answering with nothing in the logs to explain it.
			if _, err := Load(); err == nil {
				t.Fatalf("expected %s=%s to be rejected", tc.key, tc.value)
			}
		})
	}
}
