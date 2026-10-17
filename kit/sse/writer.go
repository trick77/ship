// Package sse is server-sent events: a Writer that frames and flushes events on one response, and a Hub that fans
// values out to every open stream. net/http only; it works behind httpx's middleware, whose writers flush through.
//
// A handler refuses what it must refuse (a subscriber cap, nothing to stream) before Open: Open sends the 200, and
// after it every failure is an event or a closed stream, never a status. Mount the hub's Close on shutdown
// (srv.RegisterOnShutdown(hub.Close)): an open stream is never idle, so Shutdown would otherwise wait out its timeout.
//
// A handler's loop selects on the request context, its hub channel and the Writer's Done: a client that leaves
// behind a proxy may never cancel the request, and a failed write (a keepalive, most often) is how the stream finds
// out.
package sse

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNoFlush is Open's refusal of a writer that cannot flush: every event would sit in a buffer, a slow way of
// delivering nothing. Nothing is sent, so the caller can still answer 500.
var ErrNoFlush = errors.New("sse: response writer cannot flush")

// ErrClosed is a write to a stream after Close, or after a write failed.
var ErrClosed = errors.New("sse: stream closed")

// ErrEventName is Send's refusal of an event name with a newline, which would end the event's field early.
var ErrEventName = errors.New("sse: event name with a newline")

// WriteTimeout bounds each write and its flush. httpx.NewServer sets no WriteTimeout (it would cut every stream),
// so without this a client that stops reading but keeps the connection open blocks a write, and the handler, for
// good. A write that runs past it fails and ends the stream. The deadline is cleared once the write is through:
// HTTP/2 resets a stream whose deadline passes even with nothing pending, so one left armed would cut every stream
// that stays quiet that long, and on HTTP/1.1 it would outlive the response on a keep-alive connection.
const WriteTimeout = 10 * time.Second

// Writer is one event stream. Its writes are serialized: the handler, a heartbeat and a goroutine the handler started
// may all send. Close it when the handler returns (defer): a write to a response whose handler has returned races
// net/http's own cleanup, so after Close every write is refused with ErrClosed instead. The first failed write closes
// the stream too, and Done says so.
type Writer struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	mu      sync.Mutex
	closed  bool
	done    chan struct{} // closed with the stream
	last    time.Time     // the last write, which holds the heartbeat off
	stop    chan struct{} // the heartbeat's, nil when none runs
	stopped chan struct{}
}

// Open starts the stream: the event-stream headers, the 200, and a flush, so the client's EventSource (or fetch
// reader) opens at once. Only ErrNoFlush comes before the 200; any other error is a failed flush after it, and the
// handler just returns. Cache-Control is no-store, as for every API body: a stream describes live state.
// X-Accel-Buffering: no turns off nginx's response buffering, which would batch events; Traefik does not buffer a
// response unless a buffering middleware is configured, and must not be for a stream.
func Open(w http.ResponseWriter) (*Writer, error) {
	if !canFlush(w) {
		return nil, ErrNoFlush
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	s := &Writer{w: w, rc: http.NewResponseController(w), done: make(chan struct{}), last: time.Now()}
	s.arm()
	w.WriteHeader(http.StatusOK)
	err := s.rc.Flush()
	s.disarm()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// canFlush reports whether w, or a writer it wraps, flushes: what http.ResponseController.Flush looks for, checked
// before anything is sent.
func canFlush(w http.ResponseWriter) bool {
	for {
		switch w.(type) {
		case http.Flusher, interface{ FlushError() error }:
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

// arm sets WriteTimeout for the write about to run, disarm clears it after. A writer without deadlines (a test
// recorder) has nothing to set.
func (s *Writer) arm()    { _ = s.rc.SetWriteDeadline(time.Now().Add(WriteTimeout)) }
func (s *Writer) disarm() { _ = s.rc.SetWriteDeadline(time.Time{}) }

// Send writes one event and flushes it. event "" sends an unnamed one (the client's "message"). data may hold
// newlines: each line goes out as its own data: field, which the client joins back with "\n"; a bare newline in a
// single field would end the event early. An event name with a newline is refused with ErrEventName.
func (s *Writer) Send(event, data string) error {
	if strings.ContainsAny(event, "\r\n") {
		return ErrEventName
	}
	var b strings.Builder
	if event != "" {
		b.WriteString("event: " + event + "\n")
	}
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")
	for line := range strings.SplitSeq(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(b.String())
}

// SendJSON sends v marshalled as the event's data.
func (s *Writer) SendJSON(event string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Send(event, string(body))
}

// write sends frame and flushes it, s.mu held. A failure closes the stream: the connection is no longer one an event
// can follow on.
func (s *Writer) write(frame string) error {
	if s.closed {
		return ErrClosed
	}
	s.arm()
	_, err := s.w.Write([]byte(frame))
	if err == nil {
		err = s.rc.Flush()
	}
	s.disarm()
	if err != nil {
		s.shut()
		return err
	}
	s.last = time.Now()
	return nil
}

// shut marks the stream closed and closes Done, s.mu held.
func (s *Writer) shut() {
	if !s.closed {
		s.closed = true
		close(s.done)
	}
}

// Done is closed when the stream is: by Close, or by a failed write, the heartbeat's included. A handler selects on
// it to learn that its reader left.
func (s *Writer) Done() <-chan struct{} { return s.done }

// Heartbeat sends a comment line (": keepalive") whenever the stream has been silent for interval, until the stream
// closes. Proxies drop a connection that stays quiet too long (a load balancer's idle timeout), and a write to a
// client that is gone fails, which closes the stream (Done): how a stream with nothing to send learns its reader
// left. A stream that sends more often than interval never gets one. A second call, a non-positive interval or a
// closed stream starts nothing.
func (s *Writer) Heartbeat(interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stop != nil {
		return
	}
	s.stop, s.stopped = make(chan struct{}), make(chan struct{})
	go s.beat(interval, s.stop, s.stopped)
}

// beat sleeps until interval after the last write, so the silence never exceeds interval (a fixed ticker would let
// it run to nearly twice that), and stops with the stream.
func (s *Writer) beat(interval time.Duration, stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		s.mu.Lock()
		if since := time.Since(s.last); since < interval {
			t.Reset(interval - since)
			s.mu.Unlock()
			continue
		}
		err := s.write(": keepalive\n\n")
		s.mu.Unlock()
		if err != nil {
			return
		}
		t.Reset(interval)
	}
}

// Close shuts the stream to further writes and stops the heartbeat, returning once it has stopped: every caller
// waits, so no keepalive is still writing when a handler returns. Safe to call more than once, concurrently, and
// after a failed write.
func (s *Writer) Close() {
	s.mu.Lock()
	s.shut()
	stop, stopped := s.stop, s.stopped
	s.stop = nil // stopped stays, for a later caller to wait on
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if stopped != nil {
		<-stopped
	}
}
