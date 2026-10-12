package httpx

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/trick77/ship/kit/logging"
)

// Check is one readiness dependency: a database ping, an upstream that must answer. Name is what the 503 body and
// the log report.
type Check struct {
	Name string
	Run  func(context.Context) error
}

// CheckTimeout bounds one readiness run across all checks: a dependency that hangs must fail the probe, not hold
// it until the orchestrator's own timeout reports something vaguer.
const CheckTimeout = 5 * time.Second

// DefaultReadyInterval is how long /readyz serves one run of the checks: shorter than any orchestrator's probe
// period, long enough that a probe storm on a public /readyz costs the dependencies one run per interval.
const DefaultReadyInterval = 2 * time.Second

// HealthOptions shapes HealthWith. The zero value is Health.
type HealthOptions struct {
	// ReadyInterval is how long one run's result answers /readyz, DefaultReadyInterval when zero. Negative runs the
	// checks for every probe; concurrent probes still share one run.
	ReadyInterval time.Duration
}

// Health registers the probes on mux with the default HealthOptions.
//
// GET /healthz is liveness: 200 "ok" while the process serves. It checks nothing, because a dependency outage is not
// fixed by a restart, and a liveness probe that fails on one restarts every replica into the same outage.
//
// GET /readyz is readiness: every check in order, 503 at the first failure with the check's name. The error itself
// goes to log, never the body: the probe answers unauthenticated callers, and errors carry DSNs and host names.
//
// The checks run at most once per ReadyInterval (2s): a /readyz reachable from outside must not let anyone hammer a
// database or daemon by probing it. Within the interval a probe gets the last run's result; after it, the next probe
// runs the checks and waits for them, a stale result is never served past the interval. Probes arriving during a run
// wait for that run instead of starting their own. The run belongs to no probe: a prober that hangs up stops waiting,
// the run finishes under CheckTimeout for the rest, and its result is cached all the same.
//
// The log reports transitions, not probes: an orchestrator probes every few seconds, so a line per failing probe
// buries the log for as long as a dependency is down. One warn when a check starts failing (its name and error),
// one info when the probe passes again; the same check failing again stays silent, a different one warns anew.
// AccessLog leaves both paths out (AccessLogOptions.ProbePaths): this log is their record.
func Health(mux *http.ServeMux, log *slog.Logger, checks ...Check) {
	HealthWith(mux, log, HealthOptions{}, checks...)
}

// HealthWith registers the probes Health describes, shaped by o.
func HealthWith(mux *http.ServeMux, log *slog.Logger, o HealthOptions, checks ...Check) {
	if log == nil {
		log = slog.Default()
	}
	if o.ReadyInterval == 0 {
		o.ReadyInterval = DefaultReadyInterval
	}
	rd := &readiness{checks: checks, log: log, interval: o.ReadyInterval}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", rd.serve)
}

// readiness runs the checks for /readyz: one run at a time, its result cached for interval.
type readiness struct {
	checks   []Check
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	last    readyResult
	at      time.Time     // when last was recorded; zero before the first run
	flight  chan struct{} // closed when the run in progress ends; nil when none runs
	down    bool          // the last logged transition was a failure
	failing string        // the check it named
}

type readyResult struct {
	failed bool
	check  string // the failing check's name
}

func (rd *readiness) serve(w http.ResponseWriter, r *http.Request) {
	// A prober already gone starts no run on its behalf. Answered 503 all the same: a handler that writes nothing
	// sends 200, and nothing may read as ready that no check confirmed.
	var (
		res readyResult
		ok  = r.Context().Err() == nil
	)
	if ok {
		res, ok = rd.result(r.Context())
	}
	if !ok {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	if res.failed {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "check": res.check})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// result is the cached result while fresh, else the result of the run in progress, started if none is. false: ctx
// ended before the run did.
func (rd *readiness) result(ctx context.Context) (readyResult, bool) {
	rd.mu.Lock()
	if !rd.at.IsZero() && rd.interval > 0 && time.Since(rd.at) < rd.interval {
		defer rd.mu.Unlock()
		return rd.last, true
	}
	done := rd.flight
	if done == nil {
		done = make(chan struct{})
		rd.flight = done
		// Detached from the prober: its hang-up must not fail the run every waiter shares. Values (trace ids) stay
		// for the transition log.
		go rd.run(context.WithoutCancel(ctx), done)
	}
	rd.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return readyResult{}, false
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.last, true
}

// run runs the checks, logs a transition, records the result and ends the flight.
func (rd *readiness) run(ctx context.Context, done chan struct{}) {
	ctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	var (
		res readyResult
		err error
	)
	for _, c := range rd.checks {
		if err = runCheck(ctx, c); err != nil {
			res = readyResult{failed: true, check: c.Name}
			break
		}
	}
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rd.transition(ctx, res.check, err)
	rd.last, rd.at, rd.flight = res, time.Now(), nil
	close(done)
}

// runCheck runs c, a panic counting as its failure: the run is no request's goroutine, net/http would not recover
// it, and one broken check must not take the process down.
func runCheck(ctx context.Context, c Check) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("check panicked: %v", p)
		}
	}()
	return c.Run(ctx)
}

// transition logs a change of state, under rd.mu. A nil err is ready.
func (rd *readiness) transition(ctx context.Context, check string, err error) {
	switch {
	case err != nil && (!rd.down || check != rd.failing):
		rd.log.WarnContext(ctx, "Readiness check failed", "check", check, logging.Err(err))
	case err == nil && rd.down:
		rd.log.InfoContext(ctx, "Readiness restored", "check", rd.failing)
	}
	rd.down, rd.failing = err != nil, check
}
