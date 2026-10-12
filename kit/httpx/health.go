package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/trick77/ship/kit/logging"
)

// Check is one readiness dependency: a database ping, an upstream that must answer. Name is what the 503 body and
// the log report.
type Check struct {
	Name string
	Run  func(context.Context) error
}

// CheckTimeout bounds one readiness probe across all checks: a dependency that hangs must fail the probe, not hold
// it until the orchestrator's own timeout reports something vaguer.
const CheckTimeout = 5 * time.Second

// Health registers the probes on mux.
//
// GET /healthz is liveness: 200 "ok" while the process serves. It checks nothing, because a dependency outage is not
// fixed by a restart, and a liveness probe that fails on one restarts every replica into the same outage.
//
// GET /readyz is readiness: every check in order, 503 at the first failure with the check's name. The error itself
// goes to log, never the body: the probe answers unauthenticated callers, and errors carry DSNs and host names.
func Health(mux *http.ServeMux, log *slog.Logger, checks ...Check) {
	if log == nil {
		log = slog.Default()
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), CheckTimeout)
		defer cancel()
		for _, c := range checks {
			if err := c.Run(ctx); err != nil {
				log.WarnContext(ctx, "Readiness check failed", "check", c.Name, logging.Err(err))
				WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "check": c.Name})
				return
			}
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}
