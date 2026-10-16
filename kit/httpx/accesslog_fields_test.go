package httpx

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A handler deep in the chain adds fields to the access-log line; Attrs still works next to it.
func TestLogAttrs(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var late context.Context
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		LogAttrs(r.Context(), slog.String("account_id", "acc-1"))
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() { LogAttrs(r.Context(), slog.Int("n"+strconv.Itoa(i), i)) })
		}
		wg.Wait()
		late = r.Context()
		w.WriteHeader(http.StatusTeapot)
	})
	// An outer middleware swapping the context in between must not hide the holder.
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), struct{ k int }{1}, "v")))
		})
	}
	h := Chain(inner, AccessLog(log, AccessLogOptions{Attrs: func(*http.Request, int) []slog.Attr {
		return []slog.Attr{slog.String("outer", "yes")}
	}}), wrap)
	m, _ := accessLine(t, h, &logs, httptest.NewRequest(http.MethodGet, "/x", nil))
	if m == nil {
		t.Fatal("no line")
	}
	if m["account_id"] != "acc-1" || m["outer"] != "yes" {
		t.Errorf("line = %v, want account_id and outer", m)
	}
	for i := range 20 {
		if got, ok := m["n"+strconv.Itoa(i)].(float64); !ok || int(got) != i {
			t.Errorf("n%d = %v", i, m["n"+strconv.Itoa(i)])
		}
	}
	// After the line: dropped, no panic, no race.
	LogAttrs(late, slog.String("late", "x"))
	// Outside AccessLog: a no-op.
	LogAttrs(context.Background(), slog.String("x", "y"))
}

// Level replaces the whole status rule; nil keeps the default with RoutineLevel.
func TestAccessLogLevel(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := func(opts AccessLogOptions) http.Handler {
		return AccessLog(log, opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code, _ := strconv.Atoi(r.URL.Path[1:])
			w.WriteHeader(code)
		}))
	}
	custom := AccessLogOptions{
		RoutineLevel: slog.LevelError, // ignored: Level is set
		Level: func(s int) slog.Level {
			if s == http.StatusConflict {
				return slog.LevelDebug
			}
			return StatusLevel(s, slog.LevelInfo)
		},
	}
	for _, tc := range []struct {
		opts AccessLogOptions
		path string
		want string
	}{
		{custom, "/409", "DEBUG"},
		{custom, "/403", "WARN"},
		{custom, "/500", "ERROR"},
		{custom, "/200", "INFO"},
		{custom, "/404", "INFO"},
		{AccessLogOptions{}, "/409", "WARN"},
		{AccessLogOptions{}, "/200", "DEBUG"},
		{AccessLogOptions{RoutineLevel: slog.LevelInfo}, "/401", "INFO"},
		{AccessLogOptions{Level: func(int) slog.Level { return slog.LevelInfo }}, "/500", "INFO"},
	} {
		m, _ := accessLine(t, h(tc.opts), &logs, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got, _ := m["level"].(string); got != tc.want {
			t.Errorf("%s: level %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestStatusLevel(t *testing.T) {
	for status, want := range map[int]slog.Level{
		200: slog.LevelDebug, 304: slog.LevelDebug, 401: slog.LevelDebug, 404: slog.LevelDebug,
		400: slog.LevelWarn, 409: slog.LevelWarn, 429: slog.LevelWarn, 500: slog.LevelError, 503: slog.LevelError,
	} {
		if got := StatusLevel(status, slog.LevelDebug); got != want {
			t.Errorf("StatusLevel(%d) = %v, want %v", status, got, want)
		}
	}
}

// Path masks what the line says; the handler sees the real path.
func TestAccessLogPath(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var seen string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Path })
	h := AccessLog(log, AccessLogOptions{Path: func(r *http.Request) string {
		if r.URL.Path == "/share/s3cret" {
			return "/share/{token}"
		}
		return r.URL.Path
	}})(inner)
	m, _ := accessLine(t, h, &logs, httptest.NewRequest(http.MethodGet, "/share/s3cret", nil))
	if m == nil || m["path"] != "/share/{token}" || seen != "/share/s3cret" {
		t.Errorf("line path %v, handler saw %q", m["path"], seen)
	}
}

// RecoverWith's Path keeps the secret out of the panic line as well.
func TestRecoverWithPath(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := RecoverWith(log, RecoverOptions{Path: func(*http.Request) string { return "/share/{token}" }})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/share/s3cret", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if line := logs.String(); !strings.Contains(line, `"path":"/share/{token}"`) || strings.Contains(line, "s3cret") {
		t.Errorf("panic line = %s", line)
	}
}
