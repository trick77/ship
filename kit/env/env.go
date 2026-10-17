// Package env reads process configuration from the environment and collects every fault instead of stopping at the
// first: a misconfigured deployment should not need one restart per typo to find them all.
//
// Faults come out in call order, so the same bad environment reports the same text on every start. Names are never
// prefixed or rewritten; each app owns its own variable names.
//
//	r := env.New(os.Getenv)
//	addr := r.String("BACKEND_ADDR", ":8080")
//	dsn := r.Required("DATABASE_URL")
//	if err := r.Err(); err != nil { ... }
package env

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Reader reads variables through a lookup and records every fault. Not safe for concurrent use; it lives for the
// length of one config load.
type Reader struct {
	lookup func(string) string
	errs   []error
}

// New reads through lookup: os.Getenv in production, a map in tests. A variable set to the empty string counts as
// unset: an unfilled .env.example line is the same misconfiguration as a missing one.
func New(lookup func(string) string) *Reader {
	return &Reader{lookup: lookup}
}

func (r *Reader) raw(name string) (string, bool) {
	v := strings.TrimSpace(r.lookup(name))
	return v, v != ""
}

// Fail records an app-level fault (a cross-field check, a value out of range) in sequence with the reader's own.
func (r *Reader) Fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

// String is the value of name, or def when unset.
func (r *Reader) String(name, def string) string {
	if v, ok := r.raw(name); ok {
		return v
	}
	return def
}

// Required is the value of name; unset or blank is a fault.
func (r *Reader) Required(name string) string {
	v, ok := r.raw(name)
	if !ok {
		r.Fail("%s is required", name)
	}
	return v
}

// Int is name parsed as a base-10 int, or def when unset. An unparsable value is a fault, never silently def: a
// typo'd number that boots with the default is a setting that does nothing.
func (r *Reader) Int(name string, def int) int {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.Fail("%s: %q is not an integer", name, v)
		return def
	}
	return n
}

// Float is name parsed as a float64, or def when unset. NaN and infinities are faults: no knob means them.
func (r *Reader) Float(name string, def float64) float64 {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		r.Fail("%s: %q is not a number", name, v)
		return def
	}
	return f
}

// Bool is name as true/false, 1/0, yes/no or on/off in any case, or def when unset. Anything else is a fault:
// "ture" must not read as false.
func (r *Reader) Bool(name string, def bool) bool {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	}
	r.Fail("%s: %q is not a boolean (true/false)", name, v)
	return def
}

// Duration is name parsed by time.ParseDuration ("90s", "-15m"), or whole days ("30d", "-1d"), or def when unset.
// A bare number is a fault: its unit is a guess. Days stand alone ("1d12h" is a fault): a TTL or a retention is days.
func (r *Reader) Duration(name string, def time.Duration) time.Duration {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	if d, isDays, inRange := days(v); isDays {
		if !inRange {
			r.Fail("%s: %q is out of range", name, v)
			return def
		}
		return d
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.Fail("%s: %q is not a duration (e.g. 30s, 5m, 7d)", name, v)
		return def
	}
	return d
}

// days is v as whole days, "[+-]<digits>d"; isDays is false for any other form, inRange false past time.Duration's.
func days(v string) (d time.Duration, isDays, inRange bool) {
	n, ok := strings.CutSuffix(v, "d")
	digits := strings.TrimLeft(n, "+-")
	if !ok || digits == "" || len(n)-len(digits) > 1 || strings.Trim(digits, "0123456789") != "" {
		return 0, false, false
	}
	const day = 24 * time.Hour
	c, err := strconv.ParseInt(n, 10, 64)
	if err != nil || c > int64(math.MaxInt64/day) || c < int64(math.MinInt64/day) {
		return 0, true, false
	}
	return time.Duration(c) * day, true, true
}

// URL is name parsed as an absolute http(s) URL, or def when unset; nil when both are empty. A relative URL or
// another scheme is a fault: every caller sends requests or redirects to it.
func (r *Reader) URL(name, def string) *url.URL {
	v, ok := r.raw(name)
	if !ok {
		v = def
	}
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		r.Fail("%s: %q is not an absolute http(s) URL", name, v)
		return nil
	}
	return u
}

// List is name split on commas, entries trimmed and empties dropped, or def when unset.
func (r *Reader) List(name string, def []string) []string {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Enum is name when it is one of allowed, or def when unset; any other value is a fault naming the choices (an auth
// mode, a log format). def need not be in allowed: "" for "off" is common.
func (r *Reader) Enum(name, def string, allowed ...string) string {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	if slices.Contains(allowed, v) {
		return v
	}
	r.Fail("%s: %q, want one of %s", name, v, strings.Join(allowed, ", "))
	return def
}

// Err joins every fault in call order, nil when there are none.
func (r *Reader) Err() error {
	return errors.Join(r.errs...)
}
