package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Field names Splunk treats as built-in input metadata are renamed to splunk_<key> rather than dropped: as JSON keys
// Splunk silently overwrites them with the forwarder's (source, host, index, sourcetype, time/_time/_raw) or
// double-extracts them (event).
var splunkReserved = map[string]bool{
	"source": true, "sourcetype": true, "host": true, "index": true,
	"time": true, "_time": true, "_raw": true, "event": true,
}

// Keys the handler writes itself; an attr with one of these names would collide, so it is renamed the same way.
var ownKeys = map[string]bool{
	"timestamp": true, "msg": true, "log_level": true, "service": true, "env": true,
}

// SplunkHandler writes one JSON object per line: timestamp, msg, log_level, service, env, then the context's bound
// attrs, the handler's own and the record's. Groups flatten to dotted keys. Safe for concurrent use.
type SplunkHandler struct {
	mu      *sync.Mutex
	w       io.Writer
	level   slog.Leveler
	service string
	env     string
	now     func() time.Time
	attrs   []slog.Attr
	group   string
}

// SplunkOption tunes a SplunkHandler.
type SplunkOption func(*SplunkHandler)

// WithClock injects the timestamp source for records without one; tests pin it.
func WithClock(now func() time.Time) SplunkOption { return func(h *SplunkHandler) { h.now = now } }

// NewSplunkHandler writes to w at level, stamping service and env on every line.
func NewSplunkHandler(w io.Writer, level slog.Leveler, service, env string, opts ...SplunkOption) *SplunkHandler {
	h := &SplunkHandler{mu: &sync.Mutex{}, w: w, level: level, service: service, env: env, now: time.Now}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Enabled reports whether l is at or above the handler's level.
func (h *SplunkHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

// WithAttrs returns a handler that adds attrs to every line.
func (h *SplunkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), qualify(h.group, attrs)...)
	return &c
}

// WithGroup returns a handler that prefixes later keys with name.
func (h *SplunkHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.group = qualifyKey(h.group, name)
	return &c
}

// qualify flattens a group into dotted keys: Splunk fields are flat, and a nested object hides a field from the
// extractor.
func qualify(group string, attrs []slog.Attr) []slog.Attr {
	if group == "" {
		return attrs
	}
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = slog.Attr{Key: qualifyKey(group, a.Key), Value: a.Value}
	}
	return out
}

func qualifyKey(group, key string) string {
	if group == "" {
		return key
	}
	return group + "." + key
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warning"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// Handle renders one line. Key order is deliberate: Splunk's timestamp search only looks 128 chars into the event
// and falls back to index time when it misses, so timestamp comes first, before any bound field.
func (h *SplunkHandler) Handle(ctx context.Context, r slog.Record) error {
	var buf bytes.Buffer
	buf.WriteByte('{')
	ts := r.Time
	if ts.IsZero() {
		ts = h.now()
	}
	writeField(&buf, "timestamp", ts.UTC().Format("2006-01-02T15:04:05.000000Z"), true)
	writeField(&buf, "msg", r.Message, false)
	writeField(&buf, "log_level", levelName(r.Level), false)
	writeField(&buf, "service", h.service, false)
	writeField(&buf, "env", h.env, false)

	seen := map[string]bool{}
	write := func(a slog.Attr) {
		a.Value = a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			return
		}
		key := a.Key
		if splunkReserved[key] || ownKeys[key] {
			key = "splunk_" + key
		}
		if seen[key] {
			return
		}
		seen[key] = true
		writeField(&buf, key, attrValue(a.Value), false)
	}
	// Bound fields first, latest binding winning; then WithAttrs, then the record's. A record attr never overrides
	// a bound one: the boundary owns its fields.
	for _, a := range latest(Bound(ctx)) {
		write(a)
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		write(slog.Attr{Key: qualifyKey(h.group, a.Key), Value: a.Value})
		return true
	})
	buf.WriteString("}\n")

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf.Bytes())
	return err
}

func writeField(buf *bytes.Buffer, key string, v any, first bool) {
	if !first {
		buf.WriteByte(',')
	}
	k, _ := json.Marshal(key)
	buf.Write(k)
	buf.WriteByte(':')
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(err.Error())
	}
	buf.Write(b)
}

// attrValue turns a slog value into what json.Marshal renders the way the format wants: durations as their string,
// errors as text, times in UTC, groups as objects.
func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindGroup:
		m := map[string]any{}
		for _, a := range v.Group() {
			m[a.Key] = attrValue(a.Value.Resolve())
		}
		return m
	default:
		a := v.Any()
		if err, ok := a.(error); ok {
			return err.Error()
		}
		return a
	}
}
