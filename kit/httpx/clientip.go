package httpx

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP is the request's client address. X-Forwarded-For is a list a caller can prepend anything to while each
// proxy appends the peer it saw, so the trustworthy entry is trustedHops from the right, never the leftmost. With
// trustedHops 0, or no usable header, it is the TCP peer. Use the number of proxies in front of the app: 1 behind
// a single Traefik. Every X-Forwarded-For line counts, joined in order (RFC 9110 5.3): HAProxy, the OpenShift router
// among them, adds its own line instead of appending to the caller's, and reading only the first would hand the
// caller the trusted slot.
func ClientIP(r *http.Request, trustedHops int) string {
	if trustedHops > 0 {
		if xff := strings.TrimSpace(strings.Join(r.Header.Values("X-Forwarded-For"), ",")); xff != "" {
			parts := strings.Split(xff, ",")
			if i := len(parts) - trustedHops; i >= 0 {
				if ip := strings.TrimSpace(parts[i]); ip != "" {
					return ip
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
