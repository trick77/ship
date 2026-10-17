package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestETag(t *testing.T) {
	a, b := ETag([]byte("a")), ETag([]byte("b"))
	if a == b || a != ETag([]byte("a")) {
		t.Errorf("ETag not a function of the content: %s %s", a, b)
	}
	if len(a) < 3 || a[0] != '"' || a[len(a)-1] != '"' {
		t.Errorf("ETag %s is not quoted", a)
	}
}

func TestNotModified(t *testing.T) {
	tag := ETag([]byte("body"))
	tests := []struct {
		name, method, ifNoneMatch string
		want                      bool
	}{
		{"match", http.MethodGet, tag, true},
		{"head", http.MethodHead, tag, true},
		{"weak", http.MethodGet, "W/" + tag, true},
		{"one of a list", http.MethodGet, `"other", ` + tag, true},
		{"any", http.MethodGet, "*", true},
		{"stale", http.MethodGet, `"other"`, false},
		{"none", http.MethodGet, "", false},
		{"not a read", http.MethodPost, tag, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(tt.method, "/", nil)
			if tt.ifNoneMatch != "" {
				r.Header.Set("If-None-Match", tt.ifNoneMatch)
			}
			got := NotModified(rec, r, tag)
			if got != tt.want || rec.Header().Get("ETag") != tag {
				t.Fatalf("NotModified = %v, ETag %q; want %v and %q", got, rec.Header().Get("ETag"), tt.want, tag)
			}
			if got && rec.Code != http.StatusNotModified {
				t.Errorf("status = %d, want 304", rec.Code)
			}
			if !got && rec.Body.Len() != 0 {
				t.Errorf("wrote %q before the handler's body", rec.Body)
			}
		})
	}
}
