// Package config loads and validates application configuration from environment
// variables. All settings have sane, production-oriented defaults but the
// values that matter most for a self-hosted deployment (admin password,
// storage paths, tusd internal URL) must be supplied explicitly.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for the wedding-gallery server.
type Config struct {
	// ListenAddr is the address the HTTP server listens on, e.g. ":8080".
	ListenAddr string

	// AdminPassword is the shared secret used to authenticate the single
	// admin account. There is intentionally no admin username.
	AdminPassword string

	// DataDir holds persistent application state: the SQLite database file
	// and derived thumbnails cache. Maps to the Synology docker-data mount.
	DataDir string

	// MediaDir holds the permanent original media files (photos & videos).
	// Maps to the Synology media mount.
	MediaDir string

	// TusInternalURL is the base URL of the internal tusd instance that the
	// Go backend reverse proxies to. tusd MUST NOT be reachable directly
	// from the internet; only this backend should talk to it.
	TusInternalURL string

	// TusHookSecret is a shared secret that tusd sends (and this backend
	// verifies) on every hook HTTP call, so that the hook endpoint cannot be
	// invoked by anyone else even if network isolation is ever misconfigured.
	TusHookSecret string

	// TrustedProxyCIDRs identifies reverse proxies whose client-address
	// headers may be trusted. Requests from all other peers use RemoteAddr.
	TrustedProxyCIDRs []netip.Prefix

	// MaxUploadBytes is the maximum accepted whole-file size.
	MaxUploadBytes int64

	// AllowedImageMIMEs / AllowedVideoMIMEs are the accepted content types.
	AllowedImageMIMEs []string
	AllowedVideoMIMEs []string

	// PublicRateLimitPerMinute limits general public API requests per IP.
	PublicRateLimitPerMinute int
	// PublicRateLimitBurst is the burst allowance for the public limiter.
	PublicRateLimitBurst int

	// UploadConcurrencyPerIP caps simultaneous in-flight tus PATCH requests
	// per source IP address.
	UploadConcurrencyPerIP int

	// UploadBandwidthPerIPBytesPerSec throttles upload throughput per IP to
	// keep a single guest from saturating the home connection.
	UploadBandwidthPerIPBytesPerSec int64

	// GuestNameMaxLength bounds the display name stored with each upload.
	GuestNameMaxLength int

	// CookieSecure controls the Secure flag on cookies. Should be true in
	// production (HTTPS behind Cloudflare); can be disabled for local dev.
	CookieSecure bool

	// SessionTTL is how long an admin session stays valid.
	SessionTTL time.Duration

	// ThumbnailMaxDimension is the longest edge, in pixels, of generated
	// thumbnails.
	ThumbnailMaxDimension int

	// Timezone is informational; TZ should also be set as an OS env var so
	// the Go runtime and any shelled-out tools (ffmpeg) agree on local time.
	Timezone string
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid integer for %s: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid integer for %s: %w", key, err)
	}
	return n, nil
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid boolean for %s: %w", key, err)
	}
	return b, nil
}

func envList(key string, def []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}

	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envPrefixes(key string) ([]netip.Prefix, error) {
	values := envList(key, nil)
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR for %s: %q", key, value)
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}

// Load reads configuration from the environment, applying defaults and
// validating required fields. It never reads files or performs I/O other
// than environment lookups.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:         envString("LISTEN_ADDR", ":8080"),
		AdminPassword:      os.Getenv("ADMIN_PASSWORD"),
		DataDir:            envString("DATA_DIR", "/data/app"),
		MediaDir:           envString("MEDIA_DIR", "/data/media"),
		TusInternalURL:     envString("TUS_INTERNAL_URL", "http://tusd:1080"),
		TusHookSecret:      os.Getenv("TUS_HOOK_SECRET"),
		GuestNameMaxLength: 60,
		Timezone:           envString("TZ", "UTC"),
	}

	var err error
	// Wedding guests commonly share one venue Wi-Fi/NAT address. Keep the
	// per-IP defaults deliberately generous so a whole venue is not treated
	// like one user and throttled to a handful of concurrent uploads.
	if cfg.MaxUploadBytes, err = envInt64("MAX_UPLOAD_BYTES", 5*1024*1024*1024); err != nil {
		return nil, err
	}
	if cfg.PublicRateLimitPerMinute, err = envInt("PUBLIC_RATE_LIMIT_PER_MINUTE", 12000); err != nil {
		return nil, err
	}
	if cfg.PublicRateLimitBurst, err = envInt("PUBLIC_RATE_LIMIT_BURST", 3000); err != nil {
		return nil, err
	}
	if cfg.UploadConcurrencyPerIP, err = envInt("UPLOAD_CONCURRENCY_PER_IP", 50); err != nil {
		return nil, err
	}
	if cfg.UploadBandwidthPerIPBytesPerSec, err = envInt64("UPLOAD_BANDWIDTH_PER_IP_BYTES_PER_SEC", 1024*1024*1024); err != nil {
		return nil, err
	}
	cookieSecureDefault := true
	if cfg.CookieSecure, err = envBool("COOKIE_SECURE", cookieSecureDefault); err != nil {
		return nil, err
	}
	sessionTTLMinutes, err := envInt("ADMIN_SESSION_TTL_MINUTES", 12*60)
	if err != nil {
		return nil, err
	}
	cfg.SessionTTL = time.Duration(sessionTTLMinutes) * time.Minute

	if cfg.ThumbnailMaxDimension, err = envInt("THUMBNAIL_MAX_DIMENSION", 800); err != nil {
		return nil, err
	}
	if cfg.TrustedProxyCIDRs, err = envPrefixes("TRUSTED_PROXY_CIDRS"); err != nil {
		return nil, err
	}

	cfg.AllowedImageMIMEs = envList("ALLOWED_IMAGE_MIME_TYPES", []string{
		"image/jpeg", "image/png", "image/webp", "image/gif", "image/heic", "image/heif",
	})
	cfg.AllowedVideoMIMEs = envList("ALLOWED_VIDEO_MIME_TYPES", []string{
		"video/mp4", "video/quicktime", "video/webm",
	})

	if guestNameMax, err := envInt("GUEST_NAME_MAX_LENGTH", cfg.GuestNameMaxLength); err != nil {
		return nil, err
	} else {
		cfg.GuestNameMaxLength = guestNameMax
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate ensures required configuration is present and internally
// consistent. It is separated from Load so tests can construct a Config
// directly and still validate it.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.AdminPassword) == "" {
		return fmt.Errorf("ADMIN_PASSWORD environment variable must be set")
	}
	if len(c.AdminPassword) < 8 {
		return fmt.Errorf("ADMIN_PASSWORD must be at least 8 characters long")
	}
	if strings.TrimSpace(c.TusHookSecret) == "" {
		return fmt.Errorf("TUS_HOOK_SECRET environment variable must be set")
	}
	if len(c.TusHookSecret) < 16 {
		return fmt.Errorf("TUS_HOOK_SECRET must be at least 16 characters long")
	}
	if c.MaxUploadBytes <= 0 {
		return fmt.Errorf("MAX_UPLOAD_BYTES must be positive")
	}
	if c.UploadConcurrencyPerIP <= 0 {
		return fmt.Errorf("UPLOAD_CONCURRENCY_PER_IP must be positive")
	}
	if len(c.AllowedImageMIMEs) == 0 && len(c.AllowedVideoMIMEs) == 0 {
		return fmt.Errorf("at least one allowed image or video MIME type must be configured")
	}
	return nil
}
