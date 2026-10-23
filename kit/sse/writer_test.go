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

func TestSendEventNewline(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := Open(rec)
	defer s.Close()
	if err := s.Send("a\nb", "x"); !errors.Is(err, ErrEventName) {
		t.Errorf("err = %v, want ErrEventName", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q", rec.Body)
	}
	if err := s.Send("ok", "x"); err != nil {
		t.Errorf("the refusal closed the stream: %v", err)
	}
}

func TestSendEvents(t *testing.T) {
	d := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	s, _ := Open(d)
	defer s.Close()
	opened := len(d.deadlines)
	if err := s.SendEvents([]Event{{Name: "a", Data: "1"}, {Data: "x\ny"}, {Name: "b", Data: ""}}); err != nil {
		t.Fatal(err)
	}
	if want := "event: a\ndata: 1\n\ndata: x\ndata: y\n\nevent: b\ndata: \n\n"; d.Body.String() != want {
		t.Errorf("wire = %q, want %q", d.Body, want)
	}
	// One write, one deadline armed and cleared: the batch is a single write.
	if got := len(d.deadlines) - opened; got != 2 {
		t.Errorf("%d deadlines set for one batch, want 2 (arm, disarm)", got)
	}
	if err := s.SendEvents(nil); err != nil || len(d.deadlines)-opened != 2 {
		t.Errorf("empty batch: err %v, %d deadlines", err, len(d.deadlines)-opened)
	}
	before := d.Body.Len()
	if err := s.SendEvents([]Event{{Name: "ok", Data: "1"}, {Name: "a\nb"}}); !errors.Is(err, ErrEventName) {
		t.Errorf("err = %v, want ErrEventName", err)
	}
	if d.Body.Len() != before {
		t.Errorf("a refused batch wrote %q", d.Body.String()[before:])
	}
}

// deadlineWriter records the write deadlines armed on it and fails writes once broken is set.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	broken    bool
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.broken {
		return 0, errors.New("broken pipe")
	}
	return d.ResponseRecorder.Write(b)
}

func (d *deadlineWriter) breakIt() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.broken = true
}

func TestWriteDeadlines(t *testing.T) {
	d := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	s, _ := Open(d)
	_ = s.Send("x", "y")
	s.Close()
	d.mu.Lock()
	defer d.mu.Unlock()
	// Armed for Open's flush and for Send, cleared after each: a deadline left set resets a quiet HTTP/2 stream.
	if len(d.deadlines) != 4 {
		t.Fatalf("deadlines = %v, want armed and cleared twice", d.deadlines)
	}
	for i, dl := range d.deadlines {
		if armed := i%2 == 0; armed == dl.IsZero() || (armed && time.Until(dl) > WriteTimeout) {
			t.Errorf("deadline %d = %v", i, dl)
		}
	}
}

// Every Close waits for the heartbeat, the second of two concurrent ones too.
func TestCloseConcurrent(t *testing.T) {
	for range 50 {
		s, _ := Open(syncRecorder{httptest.NewRecorder(), &sync.Mutex{}})
		s.Heartbeat(time.Microsecond)
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(s.Close)
		}
		wg.Wait()
		select {
		case <-s.stopped:
		default:
			t.Fatal("a Close returned while the heartbeat still ran")
		}
	}
}

func TestFailedWriteClosesStream(t *testing.T) {
	d := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	s, _ := Open(d)
	defer s.Close()
	d.breakIt()
	if err := s.Send("x", "y"); err == nil {
		t.Fatal("a failed write returned nil")
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("Done not closed after a failed write")
	}
	if err := s.Send("x", "y"); !errors.Is(err, ErrClosed) {
		t.Errorf("write after a failure = %v, want ErrClosed", err)
	}
}

// A keepalive to a reader that left closes the stream: the handler, idle on its hub, learns it from Done.
func TestHeartbeatFailureClosesStream(t *testing.T) {
	d := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	s, _ := Open(d)
	defer s.Close()
	d.breakIt()
	s.Heartbeat(5 * time.Millisecond)
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after a failed keepalive")
	}
}

// The silence between writes never runs past interval, wherever the last write fell.
func TestHeartbeatGap(t *testing.T) {
	rec := syncRecorder{httptest.NewRecorder(), &sync.Mutex{}}
	s, _ := Open(rec)
	// Timed from the last write the gap is about interval; from a fixed tick it would be about 1.9 intervals. The
	// bound sits between, with room for a loaded runner.
	const interval = 200 * time.Millisecond
	s.Heartbeat(interval)
	time.Sleep(interval / 10)
	_ = s.Send("x", "y") // just after where a fixed ticker would have started
	sent := time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(rec.body(), "keepalive") {
		if time.Now().After(deadline) {
			t.Fatal("no keepalive")
		}
		time.Sleep(time.Millisecond)
	}
	s.Close()
	if gap := time.Since(sent); gap > interval*3/2 {
		t.Errorf("silence ran %v, interval %v", gap, interval)
	}
}

func TestDoneOnClose(t *testing.T) {
	s, _ := Open(httptest.NewRecorder())
	s.Close()
	select {
	case <-s.Done():
	default:
		t.Error("Done open after Close")
	}
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
	s.Heartbeat(time.Second) // far above the 5ms between sends, so a stalled runner does not fire it
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
