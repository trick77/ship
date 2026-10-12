package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"DEBUG", slog.LevelDebug, false},
		{" info ", slog.LevelInfo, false},
		{"Warning", slog.LevelWarn, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"DEBUG_", 0, true},
	} {
		got, err := ParseLevel(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, %v", tt.in, got, err)
		}
		if tt.wantErr && !errors.Is(err, ErrLevel) {
			t.Errorf("ParseLevel(%q) error %v is not ErrLevel", tt.in, err)
		}
	}
}

func TestSetupRejectsUnknownLevelNamingTheVar(t *testing.T) {
	_, err := Setup(Options{Level: "loud", LevelVar: "BACKEND_LOG_LEVEL"})
	if err == nil || !strings.Contains(err.Error(), "BACKEND_LOG_LEVEL") {
		t.Fatalf("err = %v, want it to name BACKEND_LOG_LEVEL", err)
	}
	if _, err := Setup(Options{Level: "loud"}); err == nil || !strings.HasPrefix(err.Error(), "LOG_LEVEL") {
		t.Fatalf("err = %v, want LOG_LEVEL default name", err)
	}
	if _, err := Setup(Options{Format: Format(9)}); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestSetupTextCarriesBoundAttrs(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf bytes.Buffer
	l, err := Setup(Options{Level: "warn", Writer: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if slog.Default() != l {
		t.Error("Setup did not install the default")
	}
	ctx := With(context.Background(), "request_id", "r1", "team", "a")
	ctx = With(ctx, "team", "b")
	l.InfoContext(ctx, "dropped")
	l.With("k", "v").WarnContext(ctx, "kept", Err(errors.New("boom")), "x", 1)
	l.WithGroup("g").Warn("grouped", Err(errors.New("boom")))
	out := buf.String()
	if strings.Contains(out, "dropped") {
		t.Errorf("info logged at warn level: %s", out)
	}
	for _, want := range []string{"msg=kept", "k=v", "err=boom", "g.err=boom", "request_id=r1", "team=b"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "team=a") {
		t.Errorf("overridden binding leaked: %s", out)
	}
}

func TestErrNil(t *testing.T) {
	if a := Err(nil); !a.Equal(slog.Attr{}) {
		t.Errorf("Err(nil) = %v, want empty attr", a)
	}
	if a := Err(errors.New("x")); a.Key != "err" || a.Value.String() != "x" {
		t.Errorf("Err = %v", a)
	}
}

func TestWithNoAttrs(t *testing.T) {
	ctx := context.Background()
	if With(ctx) != ctx {
		t.Error("With() without attrs must return ctx unchanged")
	}
	if Bound(nil) != nil { //nolint:staticcheck // nil context is the case under test
		t.Error("Bound(nil) not nil")
	}
}

func newSplunk(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	fixed := func() time.Time { return time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC) }
	return slog.New(NewSplunkHandler(&buf, level, "svc", "test", WithClock(fixed))), &buf
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", line, err)
	}
	return m
}

func TestSplunkTimestampFirstAndSchema(t *testing.T) {
	log, buf := newSplunk(slog.LevelInfo)
	log.Info("hello", "team", "platform")
	line := strings.TrimSuffix(buf.String(), "\n")
	if !strings.HasPrefix(line, `{"timestamp":"`) {
		t.Errorf("timestamp must be the first key: %s", line)
	}
	m := decode(t, line)
	for k, want := range map[string]any{"msg": "hello", "log_level": "info", "service": "svc", "env": "test", "team": "platform"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
}

func TestSplunkLevelNames(t *testing.T) {
	log, buf := newSplunk(slog.LevelDebug)
	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		got = append(got, decode(t, line)["log_level"].(string))
	}
	if strings.Join(got, ",") != "debug,info,warning,error" {
		t.Errorf("levels = %v", got)
	}
}

func TestSplunkReservedKeysRenamed(t *testing.T) {
	log, buf := newSplunk(slog.LevelInfo)
	log.Info("x", "source", "jenkins", "host", "h", "event", "e", "timestamp", "fake", "msg", "fake")
	m := decode(t, buf.String())
	if m["splunk_source"] != "jenkins" || m["splunk_host"] != "h" || m["splunk_event"] != "e" {
		t.Errorf("reserved keys not renamed: %v", m)
	}
	if m["splunk_timestamp"] != "fake" || m["msg"] != "x" || m["splunk_msg"] != "fake" {
		t.Errorf("own keys overridden: %v", m)
	}
	if _, ok := m["source"]; ok {
		t.Error("reserved key survived")
	}
}

func TestSplunkBoundLatestWinsAndRecordNeverOverrides(t *testing.T) {
	log, buf := newSplunk(slog.LevelInfo)
	ctx := With(context.Background(), "team", "a")
	ctx = With(ctx, "request_id", "r1", "team", "b")
	log.InfoContext(ctx, "x", "team", "c")
	m := decode(t, buf.String())
	if m["team"] != "b" || m["request_id"] != "r1" {
		t.Errorf("bound = %v", m)
	}
}

func TestSplunkValueKindsGroupsAndDisabled(t *testing.T) {
	log, buf := newSplunk(slog.LevelInfo)
	log.Debug("hidden")
	log.With("a", 1).WithGroup("g").WithGroup("").Info("x",
		"dur", 1500*time.Millisecond, "at", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		"u", uint64(7), "f", 1.5, "b", true, "e", errors.New("boom"),
		slog.Group("grp", "k", "v"), "any", []int{1})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1 (debug disabled)", len(lines))
	}
	m := decode(t, lines[0])
	want := map[string]any{"a": 1.0, "g.dur": "1.5s", "g.at": "2026-01-02T03:04:05Z", "g.u": 7.0, "g.f": 1.5,
		"g.b": true, "g.e": "boom"}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
	if grp, ok := m["g.grp"].(map[string]any); !ok || grp["k"] != "v" {
		t.Errorf("group = %#v", m["g.grp"])
	}
	// WithGroup twice nests with dots.
	buf.Reset()
	log.WithGroup("a").WithGroup("b").With("c", 1).Info("y")
	if m := decode(t, buf.String()); m["a.b.c"] != 1.0 {
		t.Errorf("nested group = %v", m)
	}
}

func TestSplunkViaSetup(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf bytes.Buffer
	l, err := Setup(Options{Format: Splunk, Writer: &buf, Service: "s", Env: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("x")
	if m := decode(t, buf.String()); m["service"] != "s" || m["env"] != "prod" {
		t.Errorf("line = %v", m)
	}
}

type badJSON struct{}

func (badJSON) MarshalJSON() ([]byte, error) { return nil, errors.New("nope") }

func TestSplunkUnmarshalableValue(t *testing.T) {
	log, buf := newSplunk(slog.LevelInfo)
	log.Info("x", "v", badJSON{})
	if m := decode(t, buf.String()); !strings.Contains(m["v"].(string), "nope") {
		t.Errorf("v = %v", m["v"])
	}
}

func TestSplunkClockForZeroTime(t *testing.T) {
	var buf bytes.Buffer
	h := NewSplunkHandler(&buf, slog.LevelInfo, "s", "e",
		WithClock(func() time.Time { return time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC) }))
	if err := h.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "x", 0)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), `{"timestamp":"2026-09-19T10:00:00.000000Z"`) {
		t.Errorf("line = %s", buf.String())
	}
}
