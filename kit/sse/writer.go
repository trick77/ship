// Package sse is server-sent events: a Writer that frames and flushes events on one response, and a Hub that fans
// values out to every open stream. net/http only; it works behind httpx's middleware, whose writers flush through.
//
// A handler refuses what it must refuse (a subscriber cap, nothing to stream) before Open: Open sends the 200, and
// after it every failure is an event or a closed stream, never a status. Mount the hub's Close on shutdown
// (srv.RegisterOnShutdown(hub.Close)): an open stream is never idle, so Shutdown would otherwise wait out its timeout.
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

// ErrClosed is a write to a stream after Close.
var ErrClosed = errors.New("sse: stream closed")

// Writer is one event stream. Its writes are serialized: the handler, a heartbeat and a goroutine the handler started
// may all send. Close it when the handler returns (defer): a write to a response whose handler has returned races
// net/http's own cleanup, so after Close every write is refused with ErrClosed instead.
type Writer struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	mu      sync.Mutex
	closed  bool
	last    time.Time     // the last write, which holds the heartbeat off
	stop    chan struct{} // the heartbeat's, nil when none runs
	stopped chan struct{}
}

// Open starts the stream: the event-stream headers, the 200, and a flush, so the client's EventSource (or fetch
// reader) opens at once. Cache-Control is no-store, as for every API body: a stream describes live state. Proxy
// buffering is turned off (X-Accel-Buffering: no; nginx and Traefik honour it), which would otherwise batch events.
func Open(w http.ResponseWriter) (*Writer, error) {
	if !canFlush(w) {
		return nil, ErrNoFlush
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil, err
	}
	return &Writer{w: w, rc: rc, last: time.Now()}, nil
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

// Send writes one event and flushes it. event "" sends an unnamed one (the client's "message"). data may hold
// newlines: each line goes out as its own data: field, which the client joins back with "\n"; a bare newline in a
// single field would end the event early. An event name with a newline is the caller's bug, and panics.
func (s *Writer) Send(event, data string) error {
	if strings.ContainsAny(event, "\r\n") {
		panic("sse: event name with a newline: " + event)
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

func (s *Writer) write(frame string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if _, err := s.w.Write([]byte(frame)); err != nil {
		return err
	}
	s.last = time.Now()
	return s.rc.Flush()
}

// Heartbeat sends a comment line (": keepalive") whenever the stream has been silent for interval, until Close.
// Proxies drop a connection that stays quiet too long (Traefik, a load balancer's idle timeout), and a write to a
// client that is gone fails, which is how a stream with nothing to send learns its reader left. A stream that sends
// more often than interval never gets one. A second call, a non-positive interval or a closed stream starts nothing.
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

func (s *Writer) beat(interval time.Duration, stop <-chan struct{}, stopped chan<- struct{}) {
	defer close(stopped)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.mu.Lock()
			if !s.closed && time.Since(s.last) >= interval {
				if _, err := s.w.Write([]byte(": keepalive\n\n")); err == nil {
					s.last = time.Now()
					_ = s.rc.Flush()
				}
			}
			s.mu.Unlock()
		}
	}
}

// Close shuts the stream to further writes and stops the heartbeat, returning once it has stopped. Safe to call
// more than once.
func (s *Writer) Close() {
	s.mu.Lock()
	s.closed = true
	stop, stopped := s.stop, s.stopped
	s.stop = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
		<-stopped
	}
}
