package httpx

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"
)

// DefaultDrain is how long Main lets in-flight requests finish after the stop signal when MainOptions.Drain is not
// positive. 8s: with handlerGrace and the cleanup it stays inside Docker's and Kubernetes' default 10s stop grace.
const DefaultDrain = 8 * time.Second

// handlerGrace is the least Main waits for handlers once the server has stopped, also when the drain used up the
// whole budget: a handler whose connection the overrun's Close cut, or whose request context it ended, gets this
// moment to return before the cleanup closes what it uses. A var only for the timing test.
var handlerGrace = time.Second

// MainOptions shapes Main.
type MainOptions struct {
	// Addr is the listen address -healthcheck probes (its /healthz), required. Read it from the environment and
	// nothing else: the probe must not fail on config it does not use. Its host must be loopback ("localhost", a
	// loopback IP) or all interfaces (none, 0.0.0.0, :: in any spelling): probes answer local callers only. Build's
	// server listens on it: an empty srv.Addr becomes Addr; any other must equal Addr after splitAddr's normalization
	// (host case, any all-interfaces spelling as none, the port by number). Anything else is a startup failure.
	Addr string
	// Build makes the server, only when serving: config, secrets and the database belong here, never before Main, so
	// the probe runs without them. ctx ends at the stop; work handlers need during the drain must not stop on it — and
	// handlers that hijack (WebSockets) should watch it to end. cleanup, when not nil, runs once the server has
	// stopped and its handlers returned (close the store, stop a scheduler), also when Build fails.
	Build func(ctx context.Context) (srv *http.Server, cleanup func(), err error)
	// Drain is the stop budget, DefaultDrain when zero or negative: one deadline, counted from the stop signal, for
	// the shutdown drain and then the wait for handlers still running, which gets at least a second (handlerGrace)
	// even when the drain used it all. Set it below the orchestrator's stop grace (Docker's and Kubernetes' default
	// is 10s) by that second plus the cleanup's own time: past the grace the process is SIGKILLed and the cleanup
	// never runs.
	Drain time.Duration
	// Log gets the failure and the overrun drain; nil is slog.Default.
	Log *slog.Logger
}

// Main is a server program's main and returns its exit code: os.Exit(httpx.Main(ctx, flag.CommandLine,
// os.Args[1:], o)). It registers -healthcheck on fs, or reuses a flag of that name already there whose value reads as
// a bool (a flag.Getter, as fs.Bool's is), and parses args; with the flag set it probes o.Addr's /healthz
// (Healthcheck) and returns without building anything. Otherwise it serves o.Build's server until ctx ends or the
// process gets SIGINT/SIGTERM (Serve). Flags of the program's own go on fs before the call. Once the stop is in, or
// the server has stopped on its own, Main releases the signal handler: a further Ctrl-C or SIGTERM during a slow
// Build, the drain, the handler wait or the cleanup kills the process at once.
//
// Exit codes: 0 on a stop, also one that arrives during Build, whatever Build then returns (its error is logged), 1
// when Build or the server fails (a port that cannot bind, a server not on o.Addr) or o is unusable (no Addr, one
// not loopback or all interfaces, no Build, a -healthcheck flag that does not read as a bool), 2 on a bad command
// line. The -h (0) and bad flag (2) returns are a ContinueOnError fs's: flag.CommandLine is ExitOnError, its Parse
// exits the process with those codes itself.
//
// A drain that outlasts o.Drain closes the server (Serve), cutting off the requests still in flight, and exits 0
// with a warning: the stop was asked for and happened, it only ran long. A non-zero exit would read as a crash to
// the restart policy. Any other Shutdown error (a listener that fails to close after the drain) is a stop too: it
// is logged and exits 0. So is a Shutdown the program calls itself (an admin endpoint): that Shutdown owns its
// drain; Main waits for the handlers and runs the cleanup as on a signal.
//
// Main counts the calls into the server's handler. Once the server has stopped it waits for every call to return,
// for what is left of o.Drain since the stop but at least a second, then runs the cleanup: drain and wait share the
// one budget, so a stop takes about o.Drain before the cleanup (a second more on an overrun), never twice it.
// Shutdown waits for a connection to go idle, which is after its handler returned; only a hijacked connection (a
// WebSocket) and a handler whose connection the overrun's Close cut escape it. The count covers both, as long as the
// work runs inside the handler call. A call still running after the wait gets a warning with the count, and the
// cleanup runs under it anyway: an exit must not hang on a stuck handler. A goroutine a handler started and left
// running is not counted; it must stop on its own before the cleanup, or not use what the cleanup closes.
func Main(ctx context.Context, fs *flag.FlagSet, args []string, o MainOptions) int {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Drain <= 0 {
		o.Drain = DefaultDrain
	}
	healthcheck, ok := healthcheckFlag(fs)
	if !ok {
		o.Log.ErrorContext(ctx, "Flag -healthcheck is defined and not a bool Main can read: define -healthcheck with fs.Bool, or leave it to Main")
		return 1
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2 // fs has printed the error and its usage
	}
	if o.Addr == "" {
		// LocalURL would make it port 80: a probe of something else, or of nothing.
		o.Log.ErrorContext(ctx, "MainOptions.Addr is empty")
		return 1
	}
	host, port, err := splitAddr(o.Addr)
	switch {
	case err != nil:
		o.Log.ErrorContext(ctx, "MainOptions.Addr is not host:port", "addr", o.Addr, logging.Err(err))
		return 1
	case !probeable(host):
		o.Log.ErrorContext(ctx, "MainOptions.Addr must listen on loopback or all interfaces: probes answer local callers only", "addr", o.Addr)
		return 1
	case o.Build == nil:
		o.Log.ErrorContext(ctx, "MainOptions.Build is nil")
		return 1
	}
	if healthcheck() {
		return probe(LocalURL(o.Addr, "/healthz"))
	}
	// sig is the stop: Build's and Serve's context.
	sig, stop := run.Context(ctx)
	defer stop()
	// The handler goes as soon as the stop is in, not at return: a second signal must not wait out a slow Build, the
	// drain or the cleanup. serve takes sig as is, with no signal handling of its own.
	context.AfterFunc(sig, stop)
	srv, cleanup, err := o.Build(sig)
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if sig.Err() != nil {
		// A stop's doing, whatever Build made of it: like the drain overrun, non-zero would read as a crash to the
		// restart policy. A server Build returned anyway is not bound only to be shut down.
		level := slog.LevelInfo
		if err != nil {
			level = slog.LevelWarn
		}
		o.Log.Log(ctx, level, "Stopped during startup", logging.Err(err))
		return 0
	}
	switch {
	case err != nil:
		o.Log.ErrorContext(ctx, "Startup failed", logging.Err(err))
		return 1
	case srv == nil:
		o.Log.ErrorContext(ctx, "Startup failed: Build returned no server")
		return 1
	case srv.Addr == "":
		srv.Addr = o.Addr
	default:
		if h, p, err := splitAddr(srv.Addr); err != nil || h != host || p != port {
			// -healthcheck would probe where nothing listens, and the runtime restart a working server.
			o.Log.ErrorContext(ctx, "Startup failed: the server's Addr is not MainOptions.Addr, where -healthcheck probes",
				"addr", srv.Addr, "want", o.Addr, logging.Err(err))
			return 1
		}
	}
	var calls inflight
	srv.Handler = calls.wrap(srv.Handler)
	stoppedAt, shutdown, err := serve(sig, srv, o.Drain)
	// Release the signal handler here, not at the deferred stop: a server that failed on its own, or the program's own
	// Shutdown, left it in place, and the first signal during the handler wait or the cleanup must kill.
	stop()
	// Before the cleanup: the handlers still running would use what it closes.
	if n := calls.wait(max(time.Until(stoppedAt.Add(o.Drain)), handlerGrace)); n > 0 {
		o.Log.WarnContext(ctx, "Handlers still running after the server stopped, cleaning up under them", "handlers", n, "drain", o.Drain.String())
	}
	switch {
	case err == nil:
	case !shutdown:
		o.Log.ErrorContext(ctx, "Server failed", logging.Err(err))
		return 1
	case errors.Is(err, context.DeadlineExceeded):
		o.Log.WarnContext(ctx, "Shutdown drain exceeded, stopping with requests in flight", "drain", o.Drain.String())
	default:
		o.Log.WarnContext(ctx, "Shutdown failed after the drain", logging.Err(err))
	}
	return 0
}

// probe is Main's -healthcheck; a var only for the tests of the URL it probes.
var probe = Healthcheck

// healthcheckFlag registers -healthcheck on fs, or takes the one there when its value reads as a bool, and returns
// its reader; false when fs has a -healthcheck Main cannot read. A flag.Getter whose Get is a bool is the only
// trustworthy one: an IsBoolFlag value without it (fs.BoolFunc's) has a String that need not say whether it was set.
func healthcheckFlag(fs *flag.FlagSet) (func() bool, bool) {
	hc := fs.Lookup("healthcheck")
	if hc == nil {
		on := fs.Bool("healthcheck", false, "probe this process's /healthz and exit 0 when it answers 2xx (the container healthcheck)")
		return func() bool { return *on }, true
	}
	g, ok := hc.Value.(flag.Getter)
	if !ok {
		return nil, false
	}
	if _, ok := g.Get().(bool); !ok {
		return nil, false
	}
	return func() bool { b, _ := g.Get().(bool); return b }, true
}

// probeable reports whether a server on host, a splitAddr host, answers -healthcheck's probe: Health answers
// loopback callers only, so the host must be all interfaces ("") or loopback.
func probeable(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
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
