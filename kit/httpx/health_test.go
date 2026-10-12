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
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The health tests run in a synctest bubble: time is fake, so a test steps past the readiness cache with
// time.Sleep(DefaultReadyInterval) instantly and deterministically.

func probeReady(ctx context.Context, mux *http.ServeMux) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/readyz", nil))
	return rec
}

func TestHealth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		get := func(path string) *httptest.ResponseRecorder {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			return rec
		}
		if rec := get("/healthz"); rec.Code != 200 || rec.Body.String() != "ok\n" {
			t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
		}
		if rec := get("/readyz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok"`) {
			t.Errorf("readyz = %d %s", rec.Code, rec.Body.String())
		}
		dbDown = true
		time.Sleep(DefaultReadyInterval)
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
	})
}

// /readyz logs transitions only: the orchestrator probes every few seconds, a line per failing probe is noise.
func TestHealthLogsTransitions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
			time.Sleep(DefaultReadyInterval)
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
	})
}

// An unnamed check fails like any other; a probe whose caller already hung up runs nothing and changes nothing.
func TestHealthTransitionEdges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		probe := func(ctx context.Context) (string, int) {
			time.Sleep(DefaultReadyInterval)
			logs.Reset()
			rec := probeReady(ctx, mux)
			return logs.String(), rec.Code
		}
		if got, _ := probe(t.Context()); !strings.Contains(got, "check=db") {
			t.Fatalf("db failure not logged: %s", got)
		}
		dbDown, cacheDown = false, true
		if got, _ := probe(t.Context()); !strings.Contains(got, "cache refused") || strings.Contains(got, "restored") {
			t.Errorf("unnamed check's failure taken for ready: %s", got)
		}
		dbDown, cacheDown = true, false
		gone, cancel := context.WithCancel(t.Context())
		cancel()
		if got, code := probe(gone); got != "" || code != 503 {
			t.Errorf("hung-up probe = %d, logged: %s", code, got)
		}
		dbDown = false
		if got, _ := probe(t.Context()); !strings.Contains(got, "Readiness restored") {
			t.Errorf("restore not logged: %s", got)
		}
	})
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

// A run answers every probe for the interval, failures included; past it, the next probe runs the checks again.
func TestHealthCachesReadiness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		var down atomic.Bool
		mux := http.NewServeMux()
		Health(mux, slog.New(slog.DiscardHandler), Check{Name: "db", Run: func(context.Context) error {
			runs.Add(1)
			if down.Load() {
				return errors.New("down")
			}
			return nil
		}})
		expect := func(step string, code int, wantRuns int32) {
			t.Helper()
			if rec := probeReady(t.Context(), mux); rec.Code != code {
				t.Errorf("%s: readyz = %d, want %d", step, rec.Code, code)
			}
			if n := runs.Load(); n != wantRuns {
				t.Errorf("%s: %d runs, want %d", step, n, wantRuns)
			}
		}
		expect("first", 200, 1)
		down.Store(true)
		expect("cached", 200, 1)
		time.Sleep(DefaultReadyInterval - time.Nanosecond)
		expect("still cached", 200, 1)
		time.Sleep(time.Nanosecond)
		expect("expired", 503, 2)
		down.Store(false)
		expect("failure cached", 503, 2)
		time.Sleep(DefaultReadyInterval)
		expect("restored", 200, 3)
	})
}

// ReadyInterval sets the cache's life; negative runs the checks for every probe.
func TestHealthWithReadyInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tt := range []struct {
			interval time.Duration
			sleep    time.Duration
			want     int32
		}{
			{10 * time.Second, DefaultReadyInterval, 1},
			{10 * time.Second, 10 * time.Second, 2},
			{-1, 0, 3},
		} {
			var runs atomic.Int32
			mux := http.NewServeMux()
			HealthWith(mux, slog.New(slog.DiscardHandler), HealthOptions{ReadyInterval: tt.interval},
				Check{Name: "db", Run: func(context.Context) error { runs.Add(1); return nil }})
			probeReady(t.Context(), mux)
			time.Sleep(tt.sleep)
			probeReady(t.Context(), mux)
			probeReady(t.Context(), mux)
			if n := runs.Load(); n != tt.want {
				t.Errorf("interval %v, slept %v: %d runs, want %d", tt.interval, tt.sleep, n, tt.want)
			}
		}
	})
}

// Probes arriving during a run wait for it instead of starting their own (go test -race checks the state).
func TestHealthSharesRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		release := make(chan struct{})
		mux := http.NewServeMux()
		// Negative: no cache, so only the shared run can keep the count at one.
		HealthWith(mux, slog.New(slog.DiscardHandler), HealthOptions{ReadyInterval: -1},
			Check{Name: "db", Run: func(context.Context) error {
				runs.Add(1)
				<-release
				return nil
			}})
		const n = 20
		codes := make(chan int, n)
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() { codes <- probeReady(t.Context(), mux).Code })
		}
		synctest.Wait() // every probe is blocked: one in the check, the rest waiting on it
		if got := runs.Load(); got != 1 {
			t.Fatalf("%d runs in flight, want 1", got)
		}
		close(release)
		wg.Wait()
		close(codes)
		for c := range codes {
			if c != 200 {
				t.Errorf("readyz = %d", c)
			}
		}
		if got := runs.Load(); got != 1 {
			t.Errorf("%d runs, want 1", got)
		}
	})
}

// A prober that hangs up mid-run stops waiting with a 503; the run finishes for the others and is cached.
func TestHealthProberHangsUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		release := make(chan struct{})
		mux := http.NewServeMux()
		Health(mux, slog.New(slog.DiscardHandler), Check{Name: "db", Run: func(ctx context.Context) error {
			runs.Add(1)
			<-release
			return ctx.Err() // the run's context outlives the prober's
		}})
		ctx, cancel := context.WithCancel(t.Context())
		codes := make(chan int, 1)
		go func() { codes <- probeReady(ctx, mux).Code }()
		synctest.Wait()
		cancel()
		if c := <-codes; c != 503 {
			t.Errorf("hung-up probe = %d, want 503", c)
		}
		close(release)
		synctest.Wait()
		if rec := probeReady(t.Context(), mux); rec.Code != 200 || runs.Load() != 1 {
			t.Errorf("after the run: readyz = %d, %d runs, want 200 from the cached run", rec.Code, runs.Load())
		}
	})
}

// A check that hangs fails at CheckTimeout; one that panics fails without taking the process down.
func TestHealthCheckHangsOrPanics(t *testing.T) {
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

		logs.Reset()
		mux = http.NewServeMux()
		Health(mux, log, Check{Name: "broken", Run: func(context.Context) error { panic("nil map") }})
		rec := probeReady(t.Context(), mux)
		if rec.Code != 503 || !strings.Contains(rec.Body.String(), "broken") {
			t.Errorf("panicking check: readyz = %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(logs.String(), "check panicked: nil map") {
			t.Errorf("panic not logged: %s", logs.String())
		}
	})
}
