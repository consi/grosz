package ocpp

import (
	"net"
	"net/http"
	"strings"
)

// clientIPFromRequest returns the originating client's IP for an HTTP request,
// honoring reverse-proxy headers. Order: X-Forwarded-For (leftmost),
// X-Real-IP, then r.RemoteAddr with the :port stripped. Returns "" if
// nothing parses; callers should fall back to whatever they had.
func clientIPFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Leftmost is the original client; intermediate proxies append on the right.
		first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
		if ip := net.ParseIP(first); ip != nil {
			return ip.String()
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if ip := net.ParseIP(xri); ip != nil {
			return ip.String()
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	if ip := net.ParseIP(r.RemoteAddr); ip != nil {
		return ip.String()
	}
	return ""
}
