package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trick77/ship/kit/logging"
)

// useDefault points slog.Default at a JSON handler writing to the returned buffer for the rest of the test.
func useDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func TestServerError(t *testing.T) {
	logs := useDefault(t)
	rec := httptest.NewRecorder()
	ServerError(rec, httptest.NewRequest(http.MethodGet, "/api/s/secret-token", nil), errors.New("disk full"),
		"could not save")

	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusInternalServerError || body["error"] != "could not save" {
		t.Errorf("answer = %d %q, want 500 with the client message", rec.Code, body["error"])
	}
	if strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("the cause reached the client: %s", rec.Body)
	}
	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line: %v: %s", err, logs)
	}
	if line["level"] != "ERROR" || line["msg"] != "request failed" || line["err"] != "disk full" ||
		line["client_message"] != "could not save" {
		t.Errorf("log line = %v", line)
	}
	// The path is the access line's to log, masked; this one joins it by request id.
	if strings.Contains(logs.String(), "secret-token") {
		t.Errorf("the path reached the line: %s", logs)
	}
}

func TestServerErrorClientGone(t *testing.T) {
	logs := useDefault(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("tab closed"))
	rec := httptest.NewRecorder()
	ServerError(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), errors.New("interrupted"), "nope")

	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line: %v: %s", err, logs)
	}
	if line["level"] != "WARN" || line["msg"] != "request cancelled" || line["cause"] != "tab closed" ||
		line["err"] != "interrupted" {
		t.Errorf("log line = %v", line)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// A deadline the server set is a failure, not a client leaving.
func TestServerErrorServerTimeout(t *testing.T) {
	logs := useDefault(t)
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	ServerError(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx),
		ctx.Err(), "timed out")
	if !strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), `"msg":"request failed"`) {
		t.Errorf("log line = %s, want request failed at error", logs)
	}
}

func TestServerLogLevels(t *testing.T) {
	logs := useDefault(t)
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, tt := range []struct{ line, level string }{
		{"http: panic serving 1.2.3.4:5: boom\ngoroutine 1\n", "ERROR"},
		{"http: TLS handshake error from 1.2.3.4:5: EOF\n", "DEBUG"},
		{"http: superfluous response.WriteHeader call\n", "WARN"},
	} {
		logs.Reset()
		_, _ = serverLog{}.Write([]byte(tt.line))
		var line map[string]any
		_ = json.Unmarshal(logs.Bytes(), &line)
		if line["level"] != tt.level || strings.HasPrefix(line["err"].(string), "http: ") {
			t.Errorf("%q logged %v, want level %s without the http: prefix", tt.line, line, tt.level)
		}
	}
}

func TestServerErrorCarriesRequestID(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	// Setup's handler, so AccessLog's binding reaches the line.
	if _, err := logging.Setup(logging.Options{Writer: &logs}); err != nil {
		t.Fatal(err)
	}

	h := AccessLog(slog.New(slog.DiscardHandler), AccessLogOptions{})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { ServerError(w, r, errors.New("boom"), "failed") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	id := rec.Header().Get(RequestIDHeader)
	if id == "" || !strings.Contains(logs.String(), "request_id="+id) {
		t.Errorf("request id %q not on the line: %s", id, logs.String())
	}
}

func TestNewServerErrorLog(t *testing.T) {
	srv := NewServer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.WriteHeader(http.StatusTeapot) // net/http reports the superfluous call on ErrorLog
	}))
	// After NewServer, as a service that sets up logging late would: the writer resolves slog.Default per line.
	logs := useDefault(t)
	ts := httptest.NewUnstartedServer(srv.Handler)
	ts.Config = srv
	ts.Start()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	ts.Close()

	var line map[string]any
	if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
		t.Fatalf("log line: %v: %q", err, logs)
	}
	msg, _ := line["err"].(string)
	if line["level"] != "WARN" || line["msg"] != "http_server" || !strings.HasPrefix(msg, "superfluous") {
		t.Errorf("log line = %v", line)
	}
}
