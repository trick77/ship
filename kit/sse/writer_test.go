package sse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/ship/kit/httpx"
)

func TestOpen(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := Open(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := rec.Header()
	if rec.Code != http.StatusOK || !rec.Flushed || h.Get("Content-Type") != "text/event-stream" ||
		h.Get("Cache-Control") != "no-store" || h.Get("X-Accel-Buffering") != "no" {
		t.Errorf("status %d flushed %v header %v", rec.Code, rec.Flushed, h)
	}
}

// A writer that cannot flush is refused before anything is sent, so the caller can still answer 500.
type noFlush struct{ http.ResponseWriter }

func TestOpenWithoutFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, err := Open(noFlush{rec}); !errors.Is(err, ErrNoFlush) {
		t.Fatalf("err = %v, want ErrNoFlush", err)
	}
	if rec.Header().Get("Content-Type") != "" || rec.Body.Len() != 0 || rec.Flushed {
		t.Errorf("wrote before refusing: header %v body %q", rec.Header(), rec.Body)
	}
}

func TestSend(t *testing.T) {
	tests := []struct {
		name, event, data, want string
	}{
		{"named", "progress", `{"id":1}`, "event: progress\ndata: {\"id\":1}\n\n"},
		{"unnamed", "", "x", "data: x\n\n"},
		{"multi-line", "log", "a\nb", "event: log\ndata: a\ndata: b\n\n"},
		{"crlf", "", "a\r\nb", "data: a\ndata: b\n\n"},
		{"empty", "ping", "", "event: ping\ndata: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s, _ := Open(rec)
			defer s.Close()
			if err := s.Send(tt.event, tt.data); err != nil {
				t.Fatal(err)
			}
			if rec.Body.String() != tt.want {
				t.Errorf("wire = %q, want %q", rec.Body, tt.want)
			}
		})
	}
}

func TestSendJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := Open(rec)
	defer s.Close()
	if err := s.SendJSON("state", map[string]int{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "event: state\ndata: {\"n\":2}\n\n" {
		t.Errorf("wire = %q", rec.Body)
	}
	if err := s.SendJSON("state", func() {}); err == nil {
		t.Error("an unmarshallable value sent without error")
	}
}

func TestSendEventNewlinePanics(t *testing.T) {
	s, _ := Open(httptest.NewRecorder())
	defer s.Close()
	defer func() {
		if recover() == nil {
			t.Error("an event name with a newline did not panic")
		}
	}()
	_ = s.Send("a\nb", "x")
}

func TestCloseRefusesLaterWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := Open(rec)
	s.Close()
	s.Close() // twice is fine
	if err := s.Send("x", "y"); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after Close = %v, want ErrClosed", err)
	}
	// A goroutine the handler started (rongo's title) writing after the handler returned.
	done := make(chan error)
	go func() { done <- s.SendJSON("title", "late") }()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Errorf("late write from a goroutine = %v, want ErrClosed", err)
	}
	if strings.Contains(rec.Body.String(), "late") {
		t.Errorf("the late write reached the wire: %q", rec.Body)
	}
}

// syncRecorder is a recorder safe to read while the heartbeat writes.
type syncRecorder struct {
	*httptest.ResponseRecorder
	mu *sync.Mutex
}

func (r syncRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.Write(b)
}

func (r syncRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
}

func TestHeartbeat(t *testing.T) {
	rec := syncRecorder{httptest.NewRecorder(), &sync.Mutex{}}
	s, _ := Open(rec)
	s.Heartbeat(5 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(rec.body(), ": keepalive\n\n") {
		if time.Now().After(deadline) {
			t.Fatal("no keepalive within 2s")
		}
		time.Sleep(time.Millisecond)
	}
	s.Close() // stops the heartbeat and waits for it
	after := rec.body()
	time.Sleep(20 * time.Millisecond)
	if rec.body() != after {
		t.Error("the heartbeat wrote after Close returned")
	}
}

// Activity holds the keepalive off: a stream that sends often never gets one.
func TestHeartbeatSkipsBusyStream(t *testing.T) {
	rec := syncRecorder{httptest.NewRecorder(), &sync.Mutex{}}
	s, _ := Open(rec)
	s.Heartbeat(50 * time.Millisecond)
	for range 20 {
		_ = s.Send("tick", "x")
		time.Sleep(5 * time.Millisecond)
	}
	s.Close()
	if strings.Contains(rec.body(), "keepalive") {
		t.Errorf("keepalive on a busy stream: %q", rec.body())
	}
}

func TestHeartbeatNonPositive(_ *testing.T) {
	s, _ := Open(httptest.NewRecorder())
	s.Heartbeat(0)
	s.Close()
}

// Through kit's middleware the stream still flushes per event and the access line logs 200.
func TestStreamThroughMiddleware(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	log := slog.New(slog.NewJSONHandler(lockedWriter{&logs, &logMu}, nil))
	release := make(chan struct{})
	h := httpx.Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s, err := Open(w)
		if err != nil {
			t.Error(err)
			return
		}
		defer s.Close()
		_ = s.Send("one", "1")
		<-release
	}), httpx.AccessLog(log, httpx.AccessLogOptions{RoutineLevel: slog.LevelInfo}),
		httpx.SecurityHeadersWith(httpx.SecurityOptions{HTMLOnly: true}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The first event arrives while the handler is still blocked: it was flushed, not buffered.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "event: one\n" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Error("an event stream got the HTML-only headers")
	}
	close(release)
	_, _ = io.Copy(io.Discard, resp.Body)
	srv.Close()
	logMu.Lock()
	defer logMu.Unlock()
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry["status"] != float64(200) {
		t.Errorf("access line = %s (%v)", logs.String(), err)
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// A handler loop on a hub channel returns when the hub closes, so Shutdown does not wait out its timeout.
func TestHubCloseEndsHandler(t *testing.T) {
	hub := NewHub[string](HubOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		ch, unsub, ok := hub.Subscribe("")
		if !ok {
			return
		}
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case _, open := <-ch:
				if !open {
					return
				}
			}
		}
	}()
	for hub.Len() == 0 {
		time.Sleep(time.Millisecond)
	}
	hub.Close()
	select {
	case <-returned:
	case <-ctx.Done():
		t.Fatal("the handler loop did not return after Close")
	}
}
