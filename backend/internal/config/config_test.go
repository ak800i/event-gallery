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
