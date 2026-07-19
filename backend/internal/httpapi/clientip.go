package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// clientIP extracts the best-effort real client IP address for rate
// limiting and audit logging purposes.
//
// Proxy headers are considered only when the direct peer belongs to an
// explicitly configured trusted network.
func clientIP(r *http.Request, trustedProxies []netip.Prefix) string {
	remoteIP := remoteAddrIP(r.RemoteAddr)
	if !isTrustedProxy(remoteIP, trustedProxies) {
		return remoteIP
	}

	if cf := parseHeaderIP(r.Header.Get("CF-Connecting-IP")); cf != "" {
		return cf
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := parseHeaderIP(parts[0]); ip != "" {
			return ip
		}
	}
	return remoteIP
}

func remoteAddrIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	return host
}

func parseHeaderIP(value string) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return ip.String()
}

func isTrustedProxy(remoteIP string, trustedProxies []netip.Prefix) bool {
	ip, err := netip.ParseAddr(remoteIP)
	if err != nil {
		return false
	}
	for _, prefix := range trustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}
