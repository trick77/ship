package httpx

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
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
	// nothing else: the probe must not fail on config it does not use. Build's server listens on it: an empty
	// srv.Addr becomes Addr, a different one is a startup failure.
	Addr string
	// Build makes the server, only when serving: config, secrets and the database belong here, never before Main, so
	// the probe runs without them. ctx ends on the stop signal too, so a slow startup and the goroutines it starts
	// see it. cleanup, when not nil, runs once the server has stopped (close the store, stop a scheduler), also when
	// Build fails.
	Build func(ctx context.Context) (srv *http.Server, cleanup func(), err error)
	// Drain bounds the shutdown drain, DefaultDrain when zero or negative. It also bounds the wait for handlers still
	// running once the server has stopped, so a stop takes up to twice Drain before the cleanup.
	Drain time.Duration
	// Log gets the failure and the overrun drain; nil is slog.Default.
	Log *slog.Logger
}

// Main is a server program's main and returns its exit code: os.Exit(httpx.Main(ctx, flag.CommandLine,
// os.Args[1:], o)). It registers -healthcheck on fs, or reuses a bool flag of that name already there, and parses
// args; with the flag set it probes o.Addr's /healthz (Healthcheck) and returns without building anything.
// Otherwise it serves o.Build's server until ctx ends or the process gets SIGINT/SIGTERM (Serve); Build's ctx ends
// on the signal too. Flags of the program's own go on fs before the call. Once the stop is in, Main releases the
// signal handler: a second Ctrl-C or SIGTERM during a slow Build, drain or cleanup kills the process at once.
//
// Exit codes: 0 on a stop, also one that arrives during Build (Build then returns an error wrapping
// context.Canceled or context.DeadlineExceeded, or no server; any other error stays a failure), 1 when Build or the
// server fails (a port that cannot bind, a server not on o.Addr) or o is unusable (no Addr, no Build, a
// -healthcheck flag that is not a bool), 2 on a bad command line. The -h (0) and bad flag (2) returns are a
// ContinueOnError fs's: flag.CommandLine is ExitOnError, its Parse exits the process with those codes itself.
//
// A drain that outlasts o.Drain closes the server (Serve), cutting off the requests still in flight, and exits 0
// with a warning: the stop was asked for and happened, it only ran long. A non-zero exit would read as a crash to
// the restart policy.
//
// Main counts the calls into the server's handler. Once the server has stopped it waits up to o.Drain more for
// every call to return, then runs the cleanup. Shutdown and Close wait for neither a hijacked connection (a
// WebSocket) nor a handler's return; the count covers both, as long as the work runs inside the handler call. A
// call still running after the wait gets a warning with the count, and the cleanup runs under it anyway: an exit
// must not hang on a stuck handler. A goroutine a handler started and left running is not counted; it must stop on
// its own before the cleanup, or not use what the cleanup closes.
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
	if boolFlag(hc.Value) {
		return Healthcheck(LocalURL(o.Addr, "/healthz"))
	}
	// One signal context for Build and Serve: a stop during a slow Build reaches it, and the cleanup still runs.
	ctx, stop := run.Context(ctx)
	defer stop()
	// The handler goes as soon as the stop is in, not at return: a second signal must not wait out a drain or cleanup.
	context.AfterFunc(ctx, stop)
	srv, cleanup, err := o.Build(ctx)
	if cleanup != nil {
		defer cleanup()
	}
	// A stop's doing, not a crash: like the drain overrun, non-zero would read as one to the restart policy.
	stopped := ctx.Err() != nil
	switch {
	case err != nil && stopped && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		o.Log.InfoContext(ctx, "Stopped during startup", logging.Err(err))
		return 0
	case err != nil:
		o.Log.ErrorContext(ctx, "Startup failed", logging.Err(err))
		return 1
	case srv == nil && stopped:
		o.Log.InfoContext(ctx, "Stopped during startup")
		return 0
	case srv == nil:
		o.Log.ErrorContext(ctx, "Startup failed: Build returned no server")
		return 1
	case srv.Addr == "":
		srv.Addr = o.Addr
	case srv.Addr != o.Addr:
		// -healthcheck would probe where nothing listens, and the runtime restart a working server.
		o.Log.ErrorContext(ctx, "Startup failed: the server's Addr is not MainOptions.Addr", "addr", srv.Addr, "want", o.Addr)
		return 1
	}
	var calls inflight
	srv.Handler = calls.wrap(srv.Handler)
	err = Serve(ctx, srv, o.Drain)
	// Before the deferred cleanup: the handlers still running would use what it closes.
	if n := calls.wait(o.Drain); n > 0 {
		o.Log.WarnContext(ctx, "Handlers still running after the server stopped, cleaning up under them", "handlers", n, "drain", o.Drain.String())
	}
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

// boolFlag reports whether a bool flag is set. A flag.Getter says so itself; any other bool Value (IsBoolFlag,
// Main checked) is read from its String.
func boolFlag(v flag.Value) bool {
	if g, ok := v.(flag.Getter); ok {
		if b, ok := g.Get().(bool); ok {
			return b
		}
	}
	b, err := strconv.ParseBool(v.String())
	return err == nil && b
}

// inflight counts the handler calls running. A mutex, not a sync.WaitGroup: a request may still arrive while wait
// waits, and a WaitGroup's Add from zero must not race its Wait.
type inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n drops to zero while wait waits
}

func (c *inflight) wrap(h http.Handler) http.Handler {
	if h == nil {
		h = http.DefaultServeMux // what the server serves on a nil Handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
		defer c.done() // on a panic too: net/http recovers it above this
		h.ServeHTTP(w, r)
	})
}

func (c *inflight) done() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n--
	if c.n == 0 && c.idle != nil {
		close(c.idle)
		c.idle = nil
	}
}

// wait waits up to d for the count to reach zero and returns the calls still running.
func (c *inflight) wait(d time.Duration) int {
	c.mu.Lock()
	if c.n == 0 {
		c.mu.Unlock()
		return 0
	}
	idle := make(chan struct{})
	c.idle = idle
	c.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-idle:
	case <-t.C:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
