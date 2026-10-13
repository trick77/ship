package httpx

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"
)

// DefaultDrain is how long Main lets in-flight requests finish after the stop signal when MainOptions.Drain is not
// positive.
const DefaultDrain = 10 * time.Second

// MainOptions shapes Main.
type MainOptions struct {
	// Addr is the listen address -healthcheck probes (its /healthz), required. Read it from the environment and
	// nothing else: the probe must not fail on config it does not use.
	Addr string
	// Build makes the server, only when serving: config, secrets and the database belong here, never before Main, so
	// the probe runs without them. ctx ends on the stop signal too, so a slow startup and the goroutines it starts
	// see it. cleanup, when not nil, runs once the server has stopped (close the store, stop a scheduler), also when
	// Build fails.
	Build func(ctx context.Context) (srv *http.Server, cleanup func(), err error)
	// Drain bounds the shutdown drain, DefaultDrain when zero or negative.
	Drain time.Duration
	// Log gets the failure and the overrun drain; nil is slog.Default.
	Log *slog.Logger
}

// Main is a server program's main and returns its exit code: os.Exit(httpx.Main(ctx, flag.CommandLine,
// os.Args[1:], o)). It registers -healthcheck on fs, or reuses a bool flag of that name already there, and parses
// args; with the flag set it probes o.Addr's /healthz (Healthcheck) and returns without building anything.
// Otherwise it serves o.Build's server until ctx ends or the process gets SIGINT/SIGTERM (Serve); Build's ctx ends
// on the signal too. Flags of the program's own go on fs before the call.
//
// Exit codes: 0 on a stop, also one that arrives during Build (its error is then the stop's, not a failure), 1 when
// Build or the server fails (a port that cannot bind) or o is unusable (no Addr, no Build, a -healthcheck flag that
// is not a bool), 2 on a bad command line. The -h (0) and bad flag (2) returns are a
// ContinueOnError fs's: flag.CommandLine is ExitOnError, its Parse exits the process with those codes itself.
//
// A drain that outlasts o.Drain closes the server, cutting off the requests still in flight, and exits 0 with a
// warning: the stop was asked for and happened, it only ran long. A non-zero exit would read as a crash to the
// restart policy. The cleanup runs after the close, never under a handler still running.
func Main(ctx context.Context, fs *flag.FlagSet, args []string, o MainOptions) int {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Drain <= 0 {
		o.Drain = DefaultDrain
	}
	hc := fs.Lookup("healthcheck")
	if hc == nil {
		fs.Bool("healthcheck", false, "probe this process's /healthz and exit 0 when it answers 2xx (the container healthcheck)")
		hc = fs.Lookup("healthcheck")
	} else if b, ok := hc.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
		o.Log.ErrorContext(ctx, "Flag -healthcheck is defined and not a bool")
		return 1
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // fs has printed the error and its usage
	}
	switch {
	case o.Addr == "":
		// LocalURL would make it port 80: a probe of something else, or of nothing.
		o.Log.ErrorContext(ctx, "MainOptions.Addr is empty")
		return 1
	case o.Build == nil:
		o.Log.ErrorContext(ctx, "MainOptions.Build is nil")
		return 1
	}
	if hc.Value.String() == "true" {
		return Healthcheck(LocalURL(o.Addr, "/healthz"))
	}
	// One signal context for Build and Serve: a stop during a slow Build reaches it, and the cleanup still runs.
	ctx, stop := run.Context(ctx)
	defer stop()
	srv, cleanup, err := o.Build(ctx)
	if cleanup != nil {
		defer cleanup()
	}
	switch {
	case err != nil && ctx.Err() != nil:
		// A stop asked for, not a crash: like the drain overrun, non-zero would read as one to the restart policy.
		o.Log.InfoContext(ctx, "Stopped during startup", logging.Err(err))
		return 0
	case err != nil:
		o.Log.ErrorContext(ctx, "Startup failed", logging.Err(err))
		return 1
	case srv == nil:
		o.Log.ErrorContext(ctx, "Startup failed: Build returned no server")
		return 1
	}
	err = Serve(ctx, srv, o.Drain)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// Before the deferred cleanup: the handlers still running would use what it closes.
		_ = srv.Close()
		o.Log.WarnContext(ctx, "Shutdown drain exceeded, stopping with requests in flight", "drain", o.Drain.String())
		return 0
	case err != nil:
		o.Log.ErrorContext(ctx, "Server failed", logging.Err(err))
		return 1
	}
	return 0
}
