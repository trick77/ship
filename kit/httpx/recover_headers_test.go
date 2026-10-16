package httpx

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The 500 carries the header the outer middleware set, not what the panicking handler set or changed.
func TestRecoverResetsHeaders(t *testing.T) {
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Outer", "a")
			w.Header().Set("Vary", "Origin")
			next.ServeHTTP(w, r)
		})
	}
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="export.csv"`)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Add("X-Outer", "b")
		w.Header().Set("Vary", "Accept")
		panic("boom")
	}), AccessLog(slog.New(slog.DiscardHandler), AccessLogOptions{}), SecurityHeaders(""), outer,
		Recover(slog.New(slog.DiscardHandler)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/export", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	hd := rec.Header()
	for k, want := range map[string]string{
		"Content-Type":           "application/json",
		"Cache-Control":          "no-store",
		"Content-Disposition":    "",
		"X-Content-Type-Options": "nosniff",
		"Vary":                   "Origin",
	} {
		if got := hd.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := hd.Values("X-Outer"); len(got) != 1 || got[0] != "a" {
		t.Errorf("X-Outer = %q, want [a]", got)
	}
	if hd.Get(RequestIDHeader) == "" {
		t.Error("request id header lost")
	}
	if _, ok := hd["Content-Disposition"]; ok {
		t.Error("Content-Disposition key still present")
	}
}
