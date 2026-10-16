package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	for _, tt := range []struct {
		xff, remote string
		hops        int
		want        string
	}{
		{"", "1.2.3.4:5", 1, "1.2.3.4"},
		{"6.6.6.6, 9.9.9.9", "10.0.0.1:5", 1, "9.9.9.9"},
		{"6.6.6.6, 9.9.9.9", "10.0.0.1:5", 2, "6.6.6.6"},
		{"6.6.6.6, 9.9.9.9", "10.0.0.1:5", 0, "10.0.0.1"},
		{"9.9.9.9", "10.0.0.1:5", 3, "10.0.0.1"},
		{" , ", "10.0.0.1:5", 1, "10.0.0.1"},
		{"", "pipe", 0, "pipe"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tt.remote
		if tt.xff != "" {
			req.Header.Set("X-Forwarded-For", tt.xff)
		}
		if got := ClientIP(req, tt.hops); got != tt.want {
			t.Errorf("ClientIP(%q, %q, %d) = %q, want %q", tt.xff, tt.remote, tt.hops, got, tt.want)
		}
	}
}

// A proxy that adds its own X-Forwarded-For line (HAProxy) must not leave the caller's line in the trusted slot.
func TestClientIPJoinsEveryLine(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Add("X-Forwarded-For", "6.6.6.6")
	req.Header.Add("X-Forwarded-For", "203.0.113.9")
	if got := ClientIP(req, 1); got != "203.0.113.9" {
		t.Errorf("1 hop = %q, want the proxy's line", got)
	}
	if got := ClientIP(req, 2); got != "6.6.6.6" {
		t.Errorf("2 hops = %q, want the caller's line", got)
	}
}
