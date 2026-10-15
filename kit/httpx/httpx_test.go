package httpx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
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

func TestTooManyRequests(t *testing.T) {
	for d, want := range map[time.Duration]string{
		179500 * time.Millisecond: "180", // rounded up: 179 sends an obedient client back early
		time.Second:               "1",
		0:                         "1", // never "Retry-After: 0", which reads as retry now
		-time.Minute:              "1",
	} {
		rec := httptest.NewRecorder()
		TooManyRequests(rec, d)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != want {
			t.Errorf("%s: %d Retry-After %q, want 429 %q", d, rec.Code, rec.Header().Get("Retry-After"), want)
		}
		if !strings.Contains(rec.Body.String(), `"rate limited"`) {
			t.Errorf("%s: body = %s", d, rec.Body.String())
		}
	}
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
	failable := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") != "" {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("/status", failable)
	mux.HandleFunc("/readyz", failable)
	mux.HandleFunc("/probe", failable)
	var records []string
	opts := AccessLogOptions{TrustRequestID: true, Quiet: []string{"/status"},
		OnRecord: func(r *http.Request, _ string, _ int, _ time.Duration) { records = append(records, r.URL.Path) }}
	h := AccessLog(log, opts)(mux)

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
	if l.Level != "DEBUG" || l.Msg != "http_request" || l.Path != "/ok" || l.Status != 200 || l.RequestID != "proxy-id.1" {
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
	if l, _ = do("/401", ""); l.Level != "DEBUG" {
		t.Errorf("401 line = %+v", l)
	}
	if l, _ = do("/no-such-page", ""); l.Level != "DEBUG" || l.Status != 404 {
		t.Errorf("404 line = %+v", l)
	}
	records = nil
	if l, _ = do("/status", ""); l.Msg != "" {
		t.Errorf("quiet path logged: %+v", l)
	}
	if l, _ = do("/status?fail=1", ""); l.Status != 503 {
		t.Errorf("failing quiet path not logged: %+v", l)
	}
	// A probe is Health's to report, on the transition: never logged, not even failing.
	for _, p := range []string{"/readyz", "/readyz?fail=1"} {
		if l, _ = do(p, ""); l.Msg != "" {
			t.Errorf("probe %s logged: %+v", p, l)
		}
	}
	if !slices.Equal(records, []string{"/status"}) {
		t.Errorf("OnRecord ran for %q, want the failing quiet path only", records)
	}
	// A 404 on a probe path is a caller Health refused, logged like any 404.
	if l, _ = do("/healthz", ""); l.Status != 404 {
		t.Errorf("refused probe not logged: %+v", l)
	}
	// ProbePaths replaces the default: a probe mounted elsewhere is silent, /readyz is an ordinary path again.
	opts.ProbePaths = []string{"/probe"}
	h = AccessLog(log, opts)(mux)
	if l, _ = do("/probe?fail=1", ""); l.Msg != "" {
		t.Errorf("custom probe logged: %+v", l)
	}
	if l, _ = do("/readyz?fail=1", ""); l.Status != 503 {
		t.Errorf("/readyz outside ProbePaths not logged: %+v", l)
	}
	_ = AccessLog(nil, AccessLogOptions{})
}

// Routine lines sit at debug so LOG_LEVEL=info hides them; RoutineLevel lifts them, failures log either way.
func TestAccessLogRoutineLevel(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil)) // info, the family default
	h := func(opts AccessLogOptions) http.Handler {
		return AccessLog(log, opts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/401":
				w.WriteHeader(401)
			case "/404":
				w.WriteHeader(404)
			case "/403":
				w.WriteHeader(403)
			case "/500":
				w.WriteHeader(500)
			}
		}))
	}
	for path, want := range map[string]string{"/": "", "/401": "", "/404": "", "/403": "WARN", "/500": "ERROR"} {
		m, _ := accessLine(t, h(AccessLogOptions{}), &logs, httptest.NewRequest(http.MethodGet, path, nil))
		if got, _ := m["level"].(string); got != want {
			t.Errorf("default %s: level %q, want %q", path, got, want)
		}
	}
	for path, want := range map[string]string{"/": "INFO", "/401": "INFO", "/404": "INFO", "/403": "WARN", "/500": "ERROR"} {
		m, _ := accessLine(t, h(AccessLogOptions{RoutineLevel: slog.LevelInfo}), &logs, httptest.NewRequest(http.MethodGet, path, nil))
		if got, _ := m["level"].(string); got != want {
			t.Errorf("RoutineLevel info %s: level %q, want %q", path, got, want)
		}
	}
}

// accessLine runs one request through h and decodes the single JSON line it logged (nil when none).
func accessLine(t *testing.T, h http.Handler, logs *bytes.Buffer, req *http.Request) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	logs.Reset()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if logs.Len() == 0 {
		return nil, rec
	}
	var m map[string]any
	if err := json.Unmarshal(logs.Bytes(), &m); err != nil {
		t.Fatalf("log: %v: %s", err, logs.String())
	}
	return m, rec
}

// A header named like a request id can carry a credential (lens's gateway TOTP rides X-Request-ID): untrusted, it
// must reach neither the log, the response nor the handler's context.
func TestAccessLogUntrustedRequestID(t *testing.T) {
	const secret = "totp-123456"
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var seenID, seenHeader string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenID = RequestID(r.Context())
		seenHeader = r.Header.Get("X-Request-ID")
		for _, a := range logging.Bound(r.Context()) {
			if strings.Contains(a.Value.String(), secret) {
				t.Errorf("secret bound to the context as %s", a.Key)
			}
		}
	})
	for _, opts := range []AccessLogOptions{{}, {RequestIDHeader: "X-Request-ID"}, {NoRequestID: true}} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Request-ID", secret)
		logs.Reset()
		rec := httptest.NewRecorder()
		AccessLog(log, opts)(inner).ServeHTTP(rec, req)
		if strings.Contains(logs.String(), secret) {
			t.Errorf("%+v: secret logged: %s", opts, logs.String())
		}
		for k, vs := range rec.Header() {
			if slices.ContainsFunc(vs, func(v string) bool { return strings.Contains(v, secret) }) {
				t.Errorf("%+v: secret echoed in %s", opts, k)
			}
		}
		if seenID == secret {
			t.Errorf("%+v: secret bound as the request id", opts)
		}
		if seenHeader != secret {
			t.Errorf("%+v: handler lost the header it validates: %q", opts, seenHeader)
		}
		if opts.NoRequestID {
			if seenID != "" || rec.Header().Get("X-Request-ID") != "" || strings.Contains(logs.String(), "request_id") {
				t.Errorf("NoRequestID still produced an id: ctx %q, header %q, log %s", seenID, rec.Header().Get("X-Request-ID"), logs.String())
			}
		} else if len(seenID) != 32 || rec.Header().Get("X-Request-ID") != seenID {
			t.Errorf("%+v: own id not generated and echoed: ctx %q header %q", opts, seenID, rec.Header().Get("X-Request-ID"))
		}
	}
}

func TestAccessLogFieldsAndHooks(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	type record struct {
		path, ip string
		status   int
		dur      time.Duration
	}
	var records []record
	h := AccessLog(log, AccessLogOptions{
		RequestIDHeader: "X-Trace",
		TrustRequestID:  true,
		ClientIP:        func(r *http.Request) string { return ClientIP(r, 1) },
		Skip:            func(r *http.Request, status int) bool { return r.URL.Path == "/metrics" && status == 200 },
		Attrs:           func(_ *http.Request, status int) []slog.Attr { return []slog.Attr{slog.Bool("odd", status%2 == 1)} },
		OnRecord: func(r *http.Request, ip string, status int, dur time.Duration) {
			records = append(records, record{r.URL.Path, ip, status, dur})
		},
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(404)
		}
	}))

	req := httptest.NewRequest(http.MethodPost, "/api?token=hunter2", nil)
	req.Header.Set("X-Trace", "trace-1")
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	req.Header.Set("User-Agent", "curl/8")
	m, rec := accessLine(t, h, &logs, req)
	want := map[string]any{"request_id": "trace-1", "ip": "203.0.113.9", "method": "POST", "path": "/api",
		"proto": "HTTP/1.1", "status": float64(200), "user_agent": "curl/8", "odd": false, "msg": "http_request"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v (line %v)", k, m[k], v, m)
		}
	}
	if _, ok := m["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing: %v", m)
	}
	if strings.Contains(logs.String(), "hunter2") {
		t.Error("query logged")
	}
	if rec.Header().Get("X-Trace") != "trace-1" || rec.Header().Get(RequestIDHeader) != "" {
		t.Errorf("id header = %v", rec.Header())
	}

	if m, _ = accessLine(t, h, &logs, httptest.NewRequest(http.MethodGet, "/metrics", nil)); m != nil {
		t.Errorf("skipped request logged: %v", m)
	}
	if m, _ = accessLine(t, h, &logs, httptest.NewRequest(http.MethodGet, "/missing", nil)); m["status"] != float64(404) {
		t.Errorf("404 line = %v", m)
	}
	if len(records) != 2 || records[0].path != "/api" || records[0].ip != "203.0.113.9" || records[1].status != 404 || records[0].dur <= 0 {
		t.Errorf("OnRecord = %+v, want /api and the 404, not the skipped /metrics", records)
	}
	// Default ClientIP is the TCP peer, never X-Forwarded-For.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	m, _ = accessLine(t, AccessLog(log, AccessLogOptions{})(http.NotFoundHandler()), &logs, req)
	if m["ip"] != "192.0.2.1" {
		t.Errorf("default ip = %v", m["ip"])
	}
}

func TestChain(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "h") }), mw("a"), mw("b"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.Join(order, ","); got != "a,b,h" {
		t.Errorf("order = %s", got)
	}
}

// The status wrapper must not hide Flush from http.ResponseController, or SSE breaks behind AccessLog.
func TestAccessLogKeepsFlush(t *testing.T) {
	h := AccessLog(slog.New(slog.DiscardHandler), AccessLogOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	// The handler wrote first: its status and body stand, nothing is appended, the panic is logged.
	logs.Reset()
	wrote := Recover(slog.New(slog.NewTextHandler(&logs, nil)))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))
		panic("late")
	}))
	rec = httptest.NewRecorder()
	wrote.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Errorf("after write = %d %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "late") {
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

func TestSecurityHeadersWith(t *testing.T) {
	mw := SecurityHeadersWith(SecurityOptions{
		CSP: "default-src 'self'", HTMLOnly: true, COOP: "same-origin", PermissionsPolicy: "camera=()",
		HSTS: 2 * 365 * 24 * time.Hour, HSTSSubdomains: true,
		NoIndex: func(p string) bool { return strings.HasPrefix(p, "/api/") || p == "/healthz" },
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>"))
		case "/api/denied":
			w.Header().Set("X-Robots-Tag", "none")
			w.WriteHeader(http.StatusUnauthorized)
		case "/sniffed":
			_, _ = w.Write([]byte("<!doctype html><p>no type set"))
		case "/sniffed-text":
			_, _ = w.Write([]byte("plain words"))
		case "/untyped":
			w.WriteHeader(http.StatusTeapot)
		case "/icon.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = w.Write([]byte("<svg/>"))
		case "/page.xhtml":
			w.Header().Set("Content-Type", "Application/XHTML+XML; charset=utf-8")
			_, _ = w.Write([]byte("<html/>"))
		case "/photo.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png"))
		default:
			WriteJSON(w, http.StatusOK, map[string]string{})
		}
	}))
	get := func(p string) http.Header {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		return rec.Header()
	}
	page := get("/")
	if page.Get("Content-Security-Policy") != "default-src 'self'" || page.Get("X-Frame-Options") != "DENY" ||
		page.Get("Cross-Origin-Opener-Policy") != "same-origin" || page.Get("Permissions-Policy") != "camera=()" ||
		page.Get("X-Robots-Tag") != "" {
		t.Errorf("html = %v", page)
	}
	if got := page.Get("Strict-Transport-Security"); got != "max-age=63072000; includeSubDomains" {
		t.Errorf("HSTS = %q", got)
	}
	api := get("/api/x")
	if api.Get("Content-Security-Policy") != "" || api.Get("X-Frame-Options") != "" || api.Get("Permissions-Policy") != "" ||
		api.Get("X-Robots-Tag") != "noindex, nofollow" || api.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("json = %v", api)
	}
	if got := get("/api/denied").Get("X-Robots-Tag"); got != "none" {
		t.Errorf("handler's X-Robots-Tag = %q", got)
	}
	if got := get("/healthz").Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Errorf("probe X-Robots-Tag = %q", got)
	}
	// No Content-Type set: the sniffed type decides, and with nothing to sniff the page default holds.
	// SVG and XHTML run script like HTML; a raster image does not.
	for p, html := range map[string]bool{"/sniffed": true, "/sniffed-text": false, "/untyped": true,
		"/icon.svg": true, "/page.xhtml": true, "/photo.png": false} {
		if got := get(p).Get("Content-Security-Policy") != ""; got != html {
			t.Errorf("%s: CSP sent %v, want %v", p, got, html)
		}
	}

	// The zero value is SecurityHeaders(""): every response, no HSTS.
	rec := httptest.NewRecorder()
	SecurityHeadersWith(SecurityOptions{})(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Content-Security-Policy") != DefaultCSP || rec.Header().Get("Strict-Transport-Security") != "" ||
		rec.Header().Get("X-Robots-Tag") != "" {
		t.Errorf("zero = %v", rec.Header())
	}
}

func TestAccessLog_flushAndHijack(t *testing.T) {
	release := make(chan struct{})
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httptest.NewServer(Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			<-release
		case "/raw":
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			_ = rw.Flush()
		}
	}), AccessLog(log, AccessLogOptions{}), Recover(log)))
	defer srv.Close()
	defer close(release)

	res, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "data: one\n" {
		t.Errorf("event = %q %v", line, err)
	}
	_ = res.Body.Close()

	res, err = http.Get(srv.URL + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(body) != "hi" {
		t.Errorf("hijacked body = %q", body)
	}
}

func TestSecurityHeadersWith_whenHSTSBelowASecond_thenPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "HSTS below one second") {
			t.Errorf("recover = %v", r)
		}
	}()
	SecurityHeadersWith(SecurityOptions{HSTS: 63072000}) // seconds meant, nanoseconds given
}

func TestSecurityHeadersWith_flushAndHijack(t *testing.T) {
	release := make(chan struct{})
	mw := SecurityHeadersWith(SecurityOptions{HTMLOnly: true})
	srv := httptest.NewServer(mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			// Server-sent events: the client must see the event while the handler still runs.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: one\n\n"))
			w.(http.Flusher).Flush()
			<-release
		case "/page":
			// A flush before any write sends the header: the hook must have run by then.
			w.Header().Set("Content-Type", "text/html")
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Error(err)
			}
			<-release
		case "/raw":
			conn, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhi")
			_ = buf.Flush()
		}
	})))
	defer srv.Close()
	defer close(release)

	res, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	if err != nil || line != "data: one\n" || res.Header.Get("Content-Security-Policy") != "" {
		t.Errorf("events = %q %v %v", line, err, res.Header)
	}
	_ = res.Body.Close()

	res, err = http.Get(srv.URL + "/page")
	if err != nil {
		t.Fatal(err)
	}
	if res.Header.Get("Content-Security-Policy") != DefaultCSP {
		t.Errorf("flushed page headers = %v", res.Header)
	}
	_ = res.Body.Close()

	res, err = http.Get(srv.URL + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(body) != "hi" {
		t.Errorf("hijacked body = %q", body)
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
	if got := Healthcheck(srv.URL + "/ok"); got != 0 {
		t.Errorf("Healthcheck(/ok) = %d, want 0", got)
	}
	// The userinfo must not reach the health log.
	withSecret := strings.Replace(srv.URL, "://", "://user:secret@", 1)
	for path, want := range map[string]struct {
		code int
		out  string
	}{
		"/ok":       {0, ""},
		"/redirect": {1, "healthcheck: 302 Found\n"},
		"/down":     {1, "healthcheck: 503 Service Unavailable\n"},
	} {
		var out bytes.Buffer
		if got := healthcheck(withSecret+path, &out); got != want.code {
			t.Errorf("healthcheck(%s) = %d, want %d", path, got, want.code)
		}
		if out.String() != want.out {
			t.Errorf("healthcheck(%s) printed %q, want %q", path, out.String(), want.out)
		}
	}
	var out bytes.Buffer
	if got := healthcheck("http://127.0.0.1:1/", &out); got != 1 {
		t.Errorf("unreachable = %d", got)
	}
	// The dial error's wording is the platform's: only that a reason is printed.
	if !strings.HasPrefix(out.String(), "healthcheck: ") || len(out.String()) <= len("healthcheck: \n") {
		t.Errorf("unreachable printed %q, want the dial error", out.String())
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
	waitUp(t, "http://"+addr+"/")
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve = %v, want nil on clean shutdown", err)
	}
}

// waitUp polls url until it answers, or fails the test.
func waitUp(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for healthcheck(url, io.Discard) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holdServer serves /hold until its request's context ends or a minute passes; returned closes when it returns. up
// waits for the running server, requests /hold and returns once the handler holds it.
func holdServer(t *testing.T, addr string) (srv *http.Server, returned chan struct{}, up func()) {
	entered := make(chan struct{})
	returned = make(chan struct{})
	srv = NewServer(addr, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hold" {
			return
		}
		close(entered)
		defer close(returned)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Minute):
		}
	}))
	up = func() {
		waitUp(t, "http://"+addr+"/")
		go func() {
			resp, err := http.Get("http://" + addr + "/hold") //nolint:noctx // ends with the server
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		<-entered
	}
	return srv, returned, up
}

func TestServe_whenDrainOverruns_thenClosesAndDeadlineExceeded(t *testing.T) {
	addr := freeAddr(t)
	srv, returned, up := holdServer(t, addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, 50*time.Millisecond) }()
	up()
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Serve = %v, want context.DeadlineExceeded on an overrun", err)
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Error("the handler's connection was not closed on the overrun")
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
	if got := th.RetryAfter("a"); got != 0 {
		t.Errorf("RetryAfter of an unknown key = %s, want 0", got)
	}
	for range 3 {
		_, d, ok := th.turn("a")
		if !ok {
			t.Fatal("refused too early")
		}
		waits = append(waits, d)
	}
	if waits[0] != 0 || waits[1] != 500*time.Millisecond || waits[2] != time.Second {
		t.Errorf("waits = %v", waits)
	}
	if _, _, ok := th.turn("a"); ok {
		t.Error("queue past maxWait accepted")
	}
	// Next free slot at +1.5s, maxWait 1s: let in again in 500ms.
	if got := th.RetryAfter("a"); got != 500*time.Millisecond {
		t.Errorf("RetryAfter of a refused key = %s, want 500ms", got)
	}
	now = now.Add(500 * time.Millisecond)
	if got := th.RetryAfter("a"); got != 0 {
		t.Errorf("RetryAfter once due = %s, want 0", got)
	}
	if _, _, ok := th.turn("a"); !ok {
		t.Error("refused once RetryAfter said 0")
	}
	if _, d, ok := th.turn("b"); !ok || d != 0 {
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

// Only the latest booking is given back: with another queued behind it, moving next would hand that slot out twice.
func TestThrottle_whenLaterBookingQueued_thenReleaseKeepsIt(t *testing.T) {
	th := NewThrottle(time.Second, time.Hour)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	th.turn("k")
	a, _, _ := th.turn("k")
	b, _, _ := th.turn("k")
	th.release("k", a)
	if got := th.next["k"]; !got.Equal(b.Add(time.Second)) {
		t.Errorf("next = %v after releasing an earlier booking, want %v", got, b.Add(time.Second))
	}
	th.release("k", b)
	if got := th.next["k"]; !got.Equal(b) {
		t.Errorf("next = %v after releasing the latest booking, want its slot %v", got, b)
	}
	th.release("gone", a) // a key swept meanwhile is not recreated
	if _, ok := th.next["gone"]; ok {
		t.Error("release booked a key it never had")
	}
}

// Live entries are not rescanned on every call: the next sweep waits until the map has doubled, and the map stays
// bounded by twice the live set.
func TestThrottle_whenEntriesLive_thenSweepAmortized(t *testing.T) {
	th := NewThrottle(time.Hour, time.Hour)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	for i := range 2000 {
		th.turn(strconv.Itoa(i))
	}
	if th.pruneAt != 2*1025 {
		t.Fatalf("pruneAt = %d after a sweep that found 1025 live entries, want 2050", th.pruneAt)
	}
	now = now.Add(2 * time.Hour) // every entry stale
	for i := range 2000 {
		th.turn("new" + strconv.Itoa(i))
	}
	if n := len(th.next); n > 2*th.pruneAt {
		t.Errorf("len = %d past twice the sweep threshold %d", n, th.pruneAt)
	}
	if _, ok := th.next["0"]; ok {
		t.Error("stale entries survived the sweep")
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
	// The cancelled attempt gave its turn back: k's next one is queued at +1h again, not refused at +2h.
	queued, cancelQueued := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- th.Wait(queued, "k") }()
	for th.RetryAfter("k") == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := th.Wait(context.Background(), "k"); !errors.Is(err, ErrThrottled) {
		t.Errorf("over-queued Wait = %v", err)
	}
	cancelQueued()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("queued Wait after cancel = %v", err)
	}
	quick := NewThrottle(time.Millisecond, time.Second)
	_ = quick.Wait(context.Background(), "k")
	if err := quick.Wait(context.Background(), "k"); err != nil {
		t.Errorf("short Wait = %v", err)
	}
}
