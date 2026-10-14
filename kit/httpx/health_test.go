package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// localGet is a probe as the orchestrator sends it: from loopback, unproxied.
func localGet(ctx context.Context, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:40000"
	mux.ServeHTTP(rec, r)
	return rec
}

func probeReady(ctx context.Context, mux *http.ServeMux) *httptest.ResponseRecorder {
	return localGet(ctx, mux, "/readyz")
}

func TestHealth(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dbErr := errors.New("dial tcp db.internal:5432: refused")
	var dbDown bool
	mux := http.NewServeMux()
	Health(mux, log,
		Check{Name: "cache", Run: func(context.Context) error { return nil }},
		Check{Name: "db", Run: func(context.Context) error {
			if dbDown {
				return dbErr
			}
			return nil
		}})

	get := func(path string) *httptest.ResponseRecorder { return localGet(t.Context(), mux, path) }
	if rec := get("/healthz"); rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	if rec := get("/readyz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Errorf("readyz = %d %s", rec.Code, rec.Body.String())
	}
	dbDown = true
	if rec := get("/healthz"); rec.Code != 200 {
		t.Errorf("liveness must not fail on a dependency: %d", rec.Code)
	}
	rec := get("/readyz")
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 503 || body["check"] != "db" {
		t.Errorf("readyz = %d %v", rec.Code, body)
	}
	if strings.Contains(rec.Body.String(), "db.internal") {
		t.Error("error detail leaked into the body")
	}
	if !strings.Contains(logs.String(), "db.internal") || !strings.Contains(logs.String(), "check=db") {
		t.Errorf("failure not logged: %s", logs.String())
	}
	// nil logger falls back to the default.
	Health(http.NewServeMux(), nil)
}

// /readyz logs transitions only: the orchestrator probes every few seconds, a line per failing probe is noise.
func TestHealthLogsTransitions(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	failing := map[string]bool{}
	check := func(name string) Check {
		return Check{Name: name, Run: func(context.Context) error {
			if failing[name] {
				return errors.New(name + " refused")
			}
			return nil
		}}
	}
	mux := http.NewServeMux()
	Health(mux, log, check("cache"), check("db"))
	probe := func() []string {
		logs.Reset()
		probeReady(t.Context(), mux)
		return strings.Split(strings.TrimSpace(logs.String()), "\n")
	}
	quiet := func(step string) {
		t.Helper()
		if lines := probe(); lines[0] != "" {
			t.Errorf("%s: logged %q", step, lines)
		}
	}
	logged := func(step string, want ...string) {
		t.Helper()
		lines := probe()
		if len(lines) != 1 {
			t.Fatalf("%s: want one line, got %q", step, lines)
		}
		for _, w := range want {
			if !strings.Contains(lines[0], w) {
				t.Errorf("%s: %q lacks %q", step, lines[0], w)
			}
		}
	}

	quiet("ready")
	failing["db"] = true
	logged("db fails", "level=WARN", `msg="Readiness check failed"`, "check=db", "db refused")
	quiet("db still failing")
	quiet("db still failing")
	failing["cache"] = true
	logged("cache fails first", "level=WARN", "check=cache", "cache refused")
	failing["cache"] = false
	logged("back to db", "level=WARN", "check=db")
	failing["db"] = false
	logged("ready again", "level=INFO", `msg="Readiness restored"`, "check=db")
	quiet("still ready")
}

// An unnamed check fails like any other.
func TestHealthTransitionEdges(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dbDown, cacheDown := true, false
	mux := http.NewServeMux()
	Health(mux, log,
		Check{Name: "db", Run: func(context.Context) error {
			if dbDown {
				return errors.New("db refused")
			}
			return nil
		}},
		Check{Run: func(context.Context) error {
			if cacheDown {
				return errors.New("cache refused")
			}
			return nil
		}})
	probe := func() string {
		logs.Reset()
		probeReady(t.Context(), mux)
		return logs.String()
	}
	if got := probe(); !strings.Contains(got, "check=db") {
		t.Fatalf("db failure not logged: %s", got)
	}
	dbDown, cacheDown = false, true
	if got := probe(); !strings.Contains(got, "cache refused") || strings.Contains(got, "restored") {
		t.Errorf("unnamed check's failure taken for ready: %s", got)
	}
	cacheDown = false
	if got := probe(); !strings.Contains(got, "Readiness restored") {
		t.Errorf("restore not logged: %s", got)
	}
}

// Concurrent failing probes log the transition once (go test -race checks the state).
func TestHealthTransitionsConcurrent(t *testing.T) {
	var logs bytes.Buffer // slog's handlers serialize their writes
	log := slog.New(slog.NewTextHandler(&logs, nil))
	mux := http.NewServeMux()
	Health(mux, log, Check{Name: "db", Run: func(context.Context) error { return errors.New("down") }})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { probeReady(t.Context(), mux) })
	}
	wg.Wait()
	if n := strings.Count(logs.String(), "Readiness check failed"); n != 1 {
		t.Errorf("transition logged %d times: %s", n, logs.String())
	}
}

// A hanging check fails the probe at CheckTimeout.
func TestHealthCheckHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		mux := http.NewServeMux()
		Health(mux, log, Check{Name: "slow", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}})
		start := time.Now()
		if rec := probeReady(t.Context(), mux); rec.Code != 503 || time.Since(start) != CheckTimeout {
			t.Errorf("hanging check: readyz = %d after %v", rec.Code, time.Since(start))
		}
		if !strings.Contains(logs.String(), "deadline exceeded") {
			t.Errorf("timeout not logged: %s", logs.String())
		}
	})
}

// Only a direct caller on this host sees the probes: anyone else, or anything a proxy forwarded, gets 404.
func TestHealthOnlyLocal(t *testing.T) {
	var ran atomic.Int32
	mux := http.NewServeMux()
	Health(mux, slog.New(slog.DiscardHandler), Check{Name: "db", Run: func(context.Context) error {
		ran.Add(1)
		return nil
	}})
	for _, tc := range []struct {
		remote, header string
		want           int
	}{
		{"127.0.0.1:40000", "", 200},
		{"127.8.9.10:40000", "", 200},
		{"[::1]:40000", "", 200},
		{"[::ffff:127.0.0.1]:40000", "", 200},
		{"10.128.0.7:40000", "", 404},
		{"192.0.2.1:1234", "", 404},
		{"[2001:db8::1]:40000", "", 404},
		{"127.0.0.1", "", 404}, // no port: not a TCP peer
		{"127.0.0.1:40000", "X-Forwarded-For", 404},
		{"127.0.0.1:40000", "Forwarded", 404},
		{"127.0.0.1:40000", "X-Real-Ip", 404},
	} {
		for _, path := range []string{"/healthz", "/readyz"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.RemoteAddr = tc.remote
			if tc.header != "" {
				r.Header.Set(tc.header, "")
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Errorf("%s from %s %s = %d, want %d", path, tc.remote, tc.header, rec.Code, tc.want)
			}
		}
	}
	if n := ran.Load(); n != 4 {
		t.Errorf("checks ran %d times, want 4 (local /readyz only)", n)
	}
}

// End to end over TCP: the container's own Healthcheck passes, the same probe through a reverse proxy on this host
// (a sidecar) is refused.
func TestHealthOverLoopback(t *testing.T) {
	mux := http.NewServeMux()
	Health(mux, slog.New(slog.DiscardHandler))
	app := httptest.NewServer(mux)
	defer app.Close()
	for _, path := range []string{"/healthz", "/readyz"} {
		if got := Healthcheck(app.URL + path); got != 0 {
			t.Errorf("Healthcheck(%s) = %d, want 0", path, got)
		}
	}
	target, _ := url.Parse(app.URL)
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("%s through a proxy = %d, want 404", path, resp.StatusCode)
		}
	}
}
