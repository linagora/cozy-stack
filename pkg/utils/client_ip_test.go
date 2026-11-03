package utils

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name, remote, forwarded, want string
	}{
		{"forwarded IPv4", "192.0.2.10:80", " 198.51.100.3, 192.0.2.5", "198.51.100.3"},
		{"forwarded IPv6", "192.0.2.10:80", "2001:db8::1, 192.0.2.5", "2001:db8::1"},
		{"direct IPv4", "198.51.100.3:1234", "", "198.51.100.3"},
		{"direct IPv6", "[2001:db8::1]:1234", "", "2001:db8::1"},
		{"empty forwarded address", "[2001:db8::1]:1234", " , 192.0.2.5", "2001:db8::1"},
		{"address without port", "2001:db8::1", "", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remote
			req.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := ClientIP(req); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
