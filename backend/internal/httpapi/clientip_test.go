package httpapi

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIP_IgnoresHeadersFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("CF-Connecting-IP", "203.0.113.20")

	if got := clientIP(req, []netip.Prefix{netip.MustParsePrefix("172.30.0.0/24")}); got != "198.51.100.10" {
		t.Fatalf("expected direct peer address, got %q", got)
	}
}

func TestClientIP_TrustsCloudflareHeaderFromConfiguredProxy(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.30.0.3:1234"
	req.Header.Set("CF-Connecting-IP", "203.0.113.20")

	if got := clientIP(req, []netip.Prefix{netip.MustParsePrefix("172.30.0.0/24")}); got != "203.0.113.20" {
		t.Fatalf("expected forwarded client address, got %q", got)
	}
}

func TestClientIP_RejectsInvalidForwardedAddress(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "172.30.0.3:1234"
	req.Header.Set("CF-Connecting-IP", "not-an-ip")
	req.Header.Set("X-Forwarded-For", "also-invalid")

	if got := clientIP(req, []netip.Prefix{netip.MustParsePrefix("172.30.0.0/24")}); got != "172.30.0.3" {
		t.Fatalf("expected trusted proxy address fallback, got %q", got)
	}
}
