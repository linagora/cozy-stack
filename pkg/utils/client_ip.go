package utils

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP returns the address forwarded by the reverse proxy, or the peer's
// address when no forwarded address is provided. The proxy must sanitize the
// X-Forwarded-For header before forwarding requests to the stack.
func ClientIP(req *http.Request) string {
	if forwarded := req.Header.Get("X-Forwarded-For"); forwarded != "" {
		if ip := strings.TrimSpace(strings.SplitN(forwarded, ",", 2)[0]); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		return host
	}
	return req.RemoteAddr
}
