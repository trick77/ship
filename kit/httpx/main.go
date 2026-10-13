package httpx

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"time"

	"github.com/trick77/ship/kit/logging"
)

// DefaultDrain is how long Main lets in-flight requests finish after the stop signal when MainOptions.Drain is zero.
const DefaultDrain = 10 * time.Second

// MainOptions shapes Main.
type MainOptions struct {
	// Addr is the listen address -healthcheck probes (its /healthz). Read it from the environment and nothing else:
	// the probe must not fail on config it does not use.
	Addr string
	// Build makes the server, only when serving: config, secrets and the database belong here, never before Main, so
	// the probe runs without them. cleanup, when not nil, runs once the server has stopped (close the store, stop
	// a scheduler).
	Build func(ctx context.Context) (srv *http.Server, cleanup func(), err error)
	// Drain bounds the shutdown drain, DefaultDrain when zero.
	Drain time.Duration
	// Log gets the failure and the overrun drain; nil is slog.Default.
	Log *slog.Logger
}

// Main is a server program's main and returns its exit code: os.Exit(httpx.Main(ctx, flag.CommandLine,
// os.Args[1:], o)). It registers -healthcheck on fs and parses args; with the flag set it probes o.Addr's /healthz
// (Healthcheck) and returns without building anything. Otherwise it serves o.Build's server until ctx ends or the
// process gets SIGINT/SIGTERM (Serve). Flags of the program's own go on fs before the call.
//
// Exit codes: 0 on a stop, 1 when Build or the server fails (a port that cannot bind), 2 on a bad command line.
//
// A drain that outlasts o.Drain exits 0 with a warning: the stop was asked for and happened, it only ran long.
// A non-zero exit would read as a crash to the restart policy, and the cleanup after the server still runs.
func Main(ctx context.Context, fs *flag.FlagSet, args []string, o MainOptions) int {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Drain == 0 {
		o.Drain = DefaultDrain
	}
	healthcheck := fs.Bool("healthcheck", false, "probe this process's /healthz and exit 0 when it answers 2xx (the container healthcheck)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // fs has printed the error and its usage
	}
	if *healthcheck {
		return Healthcheck(LocalURL(o.Addr, "/healthz"))
	}
	srv, cleanup, err := o.Build(ctx)
	if err != nil {
		o.Log.ErrorContext(ctx, "Startup failed", logging.Err(err))
		return 1
	}
	if cleanup != nil {
		defer cleanup()
	}
	err = Serve(ctx, srv, o.Drain)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		o.Log.WarnContext(ctx, "Shutdown drain exceeded, stopping with requests in flight", "drain", o.Drain.String())
		return 0
	case err != nil:
		o.Log.ErrorContext(ctx, "Server failed", logging.Err(err))
		return 1
	}
	return 0
}
