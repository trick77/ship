package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ship/kit/logging"
)

func TestWriteJSONAndError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusTeapot, "nope")
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"error":"nope"}` {
		t.Errorf("body = %s", body)
	}
}

func TestHealth(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dbErr := errors.New("dial tcp db.internal:5432: refused")
	var dbDown bool
	mux := http.NewServeMux()
	Health(mux, log,
		Check{Name: "cache", Run: func(context.Context) error { return nil }},
		Check{Name: "db", Run: func(context.Context) error {
			if dbDown {
				return dbErr
			}
			return nil
		}})

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if rec := get("/healthz"); rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/readyz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("readyz = %d %s", rec.Code, rec.Body.String())
	}
	dbDown = true
	if rec := get("/healthz"); rec.Code != 200 {
		t.Errorf("liveness must not fail on a dependency: %d", rec.Code)
	}
	rec := get("/readyz")
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 503 || body["check"] != "db" {
		t.Errorf("readyz = %d %v", rec.Code, body)
	}
	if strings.Contains(rec.Body.String(), "db.internal") {
		t.Error("error detail leaked into the body")
	}
	if !strings.Contains(logs.String(), "db.internal") || !strings.Contains(logs.String(), "check=db") {
		t.Errorf("failure not logged: %s", logs.String())
	}
	// nil logger falls back to the default.
	Health(http.NewServeMux(), nil)
}

func TestAccessLog(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var seenID string
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(_ http.ResponseWriter, r *http.Request) {
		seenID = RequestID(r.Context())
		if len(logging.Bound(r.Context())) == 0 {
			t.Error("request_id not bound to the context")
		}
	})
	mux.HandleFunc("/teapot", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418); w.WriteHeader(500) })
	mux.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "x", 500) })
	mux.HandleFunc("/401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	h := AccessLog(log, "/healthz")(mux)

	type line struct {
		Level, Msg, Method, Path, RequestID string
		Status                              int
	}
	do := func(path, id string) (line, http.Header) {
		logs.Reset()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if id != "" {
			req.Header.Set(RequestIDHeader, id)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var l line
		if logs.Len() > 0 {
			var m map[string]any
			if err := json.Unmarshal(logs.Bytes(), &m); err != nil {
				t.Fatalf("log: %v: %s", err, logs.String())
			}
			l.Level, _ = m["level"].(string)
			l.Msg, _ = m["msg"].(string)
			l.Method, _ = m["method"].(string)
			l.Path, _ = m["path"].(string)
			l.RequestID, _ = m["request_id"].(string)
			if s, ok := m["status"].(float64); ok {
				l.Status = int(s)
			}
			if _, ok := m["duration_ms"].(float64); !ok {
				t.Errorf("duration_ms missing: %v", m)
			}
		}
		return l, rec.Header()
	}

	l, hdr := do("/ok?code=secret", "proxy-id.1")
	if l.Level != "INFO" || l.Msg != "http_request" || l.Path != "/ok" || l.Status != 200 || l.RequestID != "proxy-id.1" {
		t.Errorf("ok line = %+v", l)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Error("query logged")
	}
	if hdr.Get(RequestIDHeader) != "proxy-id.1" || seenID != "proxy-id.1" {
		t.Errorf("request id not passed through: header %q ctx %q", hdr.Get(RequestIDHeader), seenID)
	}
	l, hdr = do("/ok", "bad id\nforged")
	if len(l.RequestID) != 32 || hdr.Get(RequestIDHeader) != l.RequestID {
		t.Errorf("malformed id not replaced: %q", l.RequestID)
	}
	if l, _ = do("/teapot", ""); l.Level != "WARN" || l.Status != 418 {
		t.Errorf("4xx line = %+v", l)
	}
	if l, _ = do("/boom", ""); l.Level != "ERROR" || l.Status != 500 {
		t.Errorf("5xx line = %+v", l)
	}
	if l, _ = do("/401", ""); l.Level != "INFO" {
		t.Errorf("401 line = %+v", l)
	}
	if l, _ = do("/healthz", ""); l.Msg != "" {
		t.Errorf("quiet path logged: %+v", l)
	}
	if l, _ = do("/healthz?fail=1", ""); l.Status != 503 {
		t.Errorf("failing quiet path not logged: %+v", l)
	}
	_ = AccessLog(nil)
}

// The status wrapper must not hide Flush from http.ResponseController, or SSE breaks behind AccessLog.
func TestAccessLogKeepsFlush(t *testing.T) {
	h := AccessLog(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through AccessLog: %v", err)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !rec.Flushed {
		t.Error("not flushed")
	}
}

func TestRecover(t *testing.T) {
	var logs bytes.Buffer
	h := Recover(slog.New(slog.NewTextHandler(&logs, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal server error") {
		t.Errorf("recover = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "kaboom") || !strings.Contains(logs.String(), "stack=") {
		t.Errorf("log = %s", logs.String())
	}

	abort := Recover(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if r := recover(); r != http.ErrAbortHandler { //nolint:errorlint // the exact sentinel re-raised
			t.Errorf("ErrAbortHandler not re-raised: %v", r)
		}
	}()
	abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestCrossOrigin(t *testing.T) {
	h := CrossOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, tt := range []struct {
		method, site string
		want         int
	}{
		{http.MethodPost, "cross-site", http.StatusForbidden},
		{http.MethodPost, "same-origin", 204},
		{http.MethodPost, "", 204},
		{http.MethodGet, "cross-site", 204},
	} {
		req := httptest.NewRequest(tt.method, "/", nil)
		if tt.site != "" {
			req.Header.Set("Sec-Fetch-Site", tt.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s %q = %d, want %d", tt.method, tt.site, rec.Code, tt.want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	for _, csp := range []string{"", "default-src 'none'"} {
		rec := httptest.NewRecorder()
		SecurityHeaders(csp)(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		want := csp
		if want == "" {
			want = DefaultCSP
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != want {
			t.Errorf("CSP = %q, want %q", got, want)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("headers = %v", rec.Header())
		}
	}
}

func TestLocalURL(t *testing.T) {
	for _, tt := range []struct{ addr, path, want string }{
		{":8080", "/healthz", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:9000", "healthz", "http://127.0.0.1:9000/healthz"},
		{"[::]:80", "/", "http://127.0.0.1:80/"},
		{"10.0.0.1:81", "/x", "http://10.0.0.1:81/x"},
		{"localhost", "/x", "http://localhost:80/x"},
	} {
		if got := LocalURL(tt.addr, tt.path); got != tt.want {
			t.Errorf("LocalURL(%q, %q) = %q, want %q", tt.addr, tt.path, got, tt.want)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	for path, want := range map[string]int{"/ok": 0, "/redirect": 1, "/down": 1} {
		if got := Healthcheck(srv.URL + path); got != want {
			t.Errorf("Healthcheck(%s) = %d, want %d", path, got, want)
		}
	}
	if got := Healthcheck("http://127.0.0.1:1/"); got != 1 {
		t.Errorf("unreachable = %d", got)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestServeShutsDownCleanly(t *testing.T) {
	addr := freeAddr(t)
	srv := NewServer(addr, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hi")) }))
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second || srv.WriteTimeout != 0 {
		t.Errorf("timeouts = %+v", srv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, time.Second) }()
	deadline := time.Now().Add(5 * time.Second)
	for Healthcheck("http://"+addr+"/") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve = %v, want nil on clean shutdown", err)
	}
}

func TestServeBindError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := Serve(context.Background(), NewServer(l.Addr().String(), http.NotFoundHandler()), time.Second); err == nil {
		t.Error("Serve on a taken port = nil")
	}
}

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

func TestThrottle(t *testing.T) {
	th := NewThrottle(500*time.Millisecond, time.Second)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	waits := []time.Duration{}
	for range 3 {
		d, ok := th.turn("a")
		if !ok {
			t.Fatal("refused too early")
		}
		waits = append(waits, d)
	}
	if waits[0] != 0 || waits[1] != 500*time.Millisecond || waits[2] != time.Second {
		t.Errorf("waits = %v", waits)
	}
	if _, ok := th.turn("a"); ok {
		t.Error("queue past maxWait accepted")
	}
	if d, ok := th.turn("b"); !ok || d != 0 {
		t.Error("one key's flood throttled another")
	}
	// Stale entries are swept once the map is large.
	for i := range 1100 {
		th.next[string(rune(i+1000))] = now.Add(-time.Hour)
	}
	now = now.Add(time.Hour)
	th.turn("c")
	if len(th.next) > 10 {
		t.Errorf("stale entries kept: %d", len(th.next))
	}
}

func TestThrottleWait(t *testing.T) {
	th := NewThrottle(time.Hour, time.Hour)
	if err := th.Wait(context.Background(), "k"); err != nil {
		t.Fatalf("first Wait = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := th.Wait(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Wait = %v", err)
	}
	if err := th.Wait(context.Background(), "k"); !errors.Is(err, ErrThrottled) {
		t.Errorf("over-queued Wait = %v", err)
	}
	quick := NewThrottle(time.Millisecond, time.Second)
	_ = quick.Wait(context.Background(), "k")
	if err := quick.Wait(context.Background(), "k"); err != nil {
		t.Errorf("short Wait = %v", err)
	}
}
