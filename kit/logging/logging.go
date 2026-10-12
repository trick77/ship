// Package logging sets up slog once per process: the level from LOG_LEVEL-style config, a text handler by default,
// or the Splunk JSON shape for services Splunk ingests. Attrs bound to a context (With) appear on every line logged
// with that context, in either format, so a request id set at the edge reaches every line about that request.
//
// The error key is "err", always: Err builds it, so a search for one key finds every failure.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format picks the line shape.
type Format int

const (
	// Text is slog's key=value text, for humans reading a terminal or `docker logs`.
	Text Format = iota
	// Splunk is one JSON object per line, timestamp first, reserved Splunk keys renamed (see SplunkHandler).
	Splunk
)

// Options configures Setup. The zero value is info-level text on stderr.
type Options struct {
	// Level is debug, info, warn (warning) or error, any case; empty is info.
	Level string
	// LevelVar names the variable Level came from, so a bad value's error names the thing to go and fix.
	// Defaults to LOG_LEVEL.
	LevelVar string
	Format   Format
	// Writer defaults to os.Stderr.
	Writer io.Writer
	// Service and Env are stamped on every Splunk line; ignored by Text.
	Service, Env string
}

// Setup builds the logger, installs it as slog's default and returns it. Call it before the first log line, or that
// line goes out at the default level and shape.
//
// An unknown level is an error, not a silent fallback to info: a logging knob that quietly does nothing is the bug
// this replaces ("LOG_LEVEL=DEBUG_" behaving like a correct value).
func Setup(o Options) (*slog.Logger, error) {
	lvl, err := ParseLevel(o.Level)
	if err != nil {
		name := o.LevelVar
		if name == "" {
			name = "LOG_LEVEL"
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	w := o.Writer
	if w == nil {
		w = os.Stderr
	}
	var h slog.Handler
	switch o.Format {
	case Text:
		h = contextHandler{slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})}
	case Splunk:
		h = NewSplunkHandler(w, lvl, o.Service, o.Env)
	default:
		return nil, fmt.Errorf("unknown log format %d", o.Format)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l, nil
}

// ErrLevel is the error ParseLevel wraps for a value it does not know.
var ErrLevel = errors.New("not one of debug, info, warn, error")

// ParseLevel reads debug, info, warn/warning or error in any case; empty is info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%q is %w", s, ErrLevel)
}

// Err is the canonical error attr: key "err". A nil error yields an empty attr, which slog drops.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String("err", err.Error())
}

type ctxKey struct{}

// With returns a context whose log lines carry attrs (key-value pairs or slog.Attr, as slog.Logger.Info takes).
// Bound at each boundary (request, queue worker) so call sites do not repeat them. A later binding wins over an
// earlier one with the same key.
func With(ctx context.Context, attrs ...any) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	parent := Bound(ctx)
	bound := make([]slog.Attr, 0, len(parent)+len(attrs))
	bound = append(bound, parent...)
	var rec slog.Record
	rec.Add(attrs...)
	rec.Attrs(func(a slog.Attr) bool {
		bound = append(bound, a)
		return true
	})
	return context.WithValue(ctx, ctxKey{}, bound)
}

// Bound returns the attrs a context carries, latest binding last.
func Bound(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(ctxKey{}).([]slog.Attr)
	return attrs
}

// latest drops every bound attr a later binding with the same key overrides, keeping first-binding order.
func latest(bound []slog.Attr) []slog.Attr {
	last := make(map[string]int, len(bound))
	for i, a := range bound {
		last[a.Key] = i
	}
	out := make([]slog.Attr, 0, len(last))
	for i, a := range bound {
		if last[a.Key] == i {
			out = append(out, a)
		}
	}
	return out
}

// contextHandler adds a context's bound attrs to a wrapped handler's records.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if bound := Bound(ctx); len(bound) > 0 {
		r = r.Clone()
		r.AddAttrs(latest(bound)...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
