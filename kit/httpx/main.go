package httpx

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
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
	// nothing else: the probe must not fail on config it does not use. Build's server listens on it: an empty
	// srv.Addr becomes Addr; any other must have Addr's port (by number) and a host the probe of Addr reaches: no
	// host or :: (any spelling) takes every probe, 0.0.0.0 every probe but an IPv6 literal (a hostname in Addr, such
	// as "app:8080", resolves to IPv4 there), localhost and 127.0.0.1 a probe of localhost, 127.0.0.1 or a wildcard,
	// ::1 one of localhost or ::1, any other host only the same host. Anything else is a startup failure.
	Addr string
	// Build makes the server, only when serving: config, secrets and the database belong here, never before Main, so
	// the probe runs without them. ctx lives until the server has drained; start background work on it. A stop
	// signal while Build runs ends ctx too, so a slow startup sees it; one after Build returned leaves ctx to the
	// drain and the handler wait, and Main ends it right before the cleanup. cleanup, when not nil, runs once the
	// server has stopped (close the store, stop a scheduler), also when Build fails.
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
// Exit codes: 0 on a stop, also one that arrives during Build (Build then returns an error wrapping
// context.Canceled or context.DeadlineExceeded, no server, or a server Main then never binds; any other error stays a
// failure), 1 when Build or the server fails (a port that cannot bind, a server not on o.Addr) or o is unusable (no
// Addr, no Build, a -healthcheck flag that does not read as a bool), 2 on a bad command line. The -h (0) and bad flag
// (2) returns are a ContinueOnError fs's: flag.CommandLine is ExitOnError, its Parse exits the process with those
// codes itself.
//
// A drain that outlasts o.Drain closes the server (Serve), cutting off the requests still in flight, and exits 0
// with a warning: the stop was asked for and happened, it only ran long. A non-zero exit would read as a crash to
// the restart policy. Any other Shutdown error (a listener that fails to close after the drain) is a stop too: it
// is logged and exits 0. So is a Shutdown the program calls itself (an admin endpoint): Main joins its drain, waits
// for the handlers and runs the cleanup as on a signal.
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
	switch {
	case o.Addr == "":
		// LocalURL would make it port 80: a probe of something else, or of nothing.
		o.Log.ErrorContext(ctx, "MainOptions.Addr is empty")
		return 1
	case o.Build == nil:
		o.Log.ErrorContext(ctx, "MainOptions.Build is nil")
		return 1
	}
	if healthcheck() {
		return probe(LocalURL(o.Addr, "/healthz"))
	}
	// sig is the stop: Serve's context.
	sig, stop := run.Context(ctx)
	defer stop()
	// The handler goes as soon as the stop is in, not at return: a second signal must not wait out a slow Build, the
	// drain or the cleanup. serve takes sig as is, with no signal handling of its own.
	context.AfterFunc(sig, stop)
	// life is Build's context, the lifetime of what Build starts. The stop ends it only while Build runs, so a slow
	// startup sees it; after that it outlives the drain and the handler wait, whose handlers may need what it runs,
	// and ends right before the cleanup.
	life, endLife := context.WithCancel(context.WithoutCancel(ctx))
	defer endLife()
	endOnStop := context.AfterFunc(sig, endLife)
	srv, cleanup, err := o.Build(life)
	endOnStop()
	defer func() {
		stop() // the server is done or never ran: a signal during the cleanup kills
		endLife()
		if cleanup != nil {
			cleanup()
		}
	}()
	// A stop's doing, not a crash: like the drain overrun, non-zero would read as one to the restart policy.
	stopped := sig.Err() != nil
	stopsOwn := stopped && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
	switch {
	case err != nil && !stopsOwn:
		o.Log.ErrorContext(ctx, "Startup failed", logging.Err(err))
		return 1
	case err != nil: // the stop's own error
	case srv == nil && !stopped:
		o.Log.ErrorContext(ctx, "Startup failed: Build returned no server")
		return 1
	case srv == nil:
	case srv.Addr == "":
		srv.Addr = o.Addr
	default:
		if err := probeReaches(srv.Addr, o.Addr); err != nil {
			// -healthcheck would probe where nothing listens, and the runtime restart a working server.
			o.Log.ErrorContext(ctx, "Startup failed: the server's Addr is not where -healthcheck probes MainOptions.Addr",
				"addr", srv.Addr, "want", o.Addr, logging.Err(err))
			return 1
		}
	}
	if stopped {
		// No server, the stop's own error, or a server Build returned ignoring ctx: binding it only to shut it down
		// helps no one.
		o.Log.InfoContext(ctx, "Stopped during startup", logging.Err(err))
		return 0
	}
	var calls inflight
	srv.Handler = calls.wrap(srv.Handler)
	stoppedAt, stopped, err := serve(sig, srv, o.Drain)
	// A server that failed on its own, or the program's own Shutdown, left the handler in place: the first signal
	// during the handler wait or the cleanup must kill.
	stop()
	// Before the cleanup: the handlers still running would use what it closes.
	if n := calls.wait(max(time.Until(stoppedAt.Add(o.Drain)), handlerGrace)); n > 0 {
		o.Log.WarnContext(ctx, "Handlers still running after the server stopped, cleaning up under them", "handlers", n, "drain", o.Drain.String())
	}
	switch {
	case err != nil && !stopped:
		o.Log.ErrorContext(ctx, "Server failed", logging.Err(err))
		return 1
	case errors.Is(err, context.DeadlineExceeded):
		o.Log.WarnContext(ctx, "Shutdown drain exceeded, stopping with requests in flight", "drain", o.Drain.String())
		return 0
	case err != nil:
		o.Log.WarnContext(ctx, "Shutdown failed after the drain", logging.Err(err))
		return 0
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

// probeReaches returns nil when a server listening on addr answers -healthcheck's probe of want, else why not: the
// same port (by number: ":http" is ":80", ":08080" is ":8080"), and addr's host takes a connection to LocalURL's host
// (hostReaches).
func probeReaches(addr, want string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	wantHost, wantPort, err := net.SplitHostPort(want)
	if err != nil {
		return err
	}
	p, err := net.LookupPort("tcp", port)
	if err != nil {
		return err
	}
	wp, err := net.LookupPort("tcp", wantPort)
	if err != nil {
		return err
	}
	if p != wp {
		return fmt.Errorf("port %d, the probe dials %d", p, wp)
	}
	if !hostReaches(host, dialHost(wantHost)) {
		return fmt.Errorf("host %q does not take the probe's connection to %q", host, dialHost(wantHost))
	}
	return nil
}

// hostReaches reports whether a server listening on host takes a connection to probe, a dialHost. IPs compare as
// addresses, not spellings, and the probe dials exactly probe: a loopback server reaches only its own address
// (127.0.0.2 is not 127.0.0.1). A probe of "localhost" dials ::1 and 127.0.0.1; a server on "localhost" binds
// 127.0.0.1 (Go's Listen prefers IPv4). A hostname takes only the same hostname, or a wildcard.
func hostReaches(host, probe string) bool {
	if host == probe {
		return true
	}
	switch host {
	case "":
		host = "::"
	case "localhost":
		host = "127.0.0.1"
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false // a hostname, not probe's
	}
	ip = ip.Unmap()
	var pip netip.Addr // invalid: probe is a hostname
	if p, err := netip.ParseAddr(probe); err == nil {
		pip = p.Unmap()
	}
	switch {
	// Dual-stack: a "tcp" wildcard listen is AF_INET6 with IPV6_V6ONLY set to 0 whatever the OS default (Go's
	// net.favoriteAddrFamily, setDefaultSockopts); not on OpenBSD and DragonFly, which have no IPv4-mapped IPv6.
	case ip.IsUnspecified() && ip.Is6():
		return true
	// IPv4 only: an IPv6 literal misses it; a hostname must resolve to IPv4 to reach it, as Addr's doc demands.
	case ip.IsUnspecified():
		return !pip.IsValid() || pip.Is4()
	case probe == "localhost":
		return ip == netip.AddrFrom4([4]byte{127, 0, 0, 1}) || ip == netip.IPv6Loopback()
	}
	return pip.IsValid() && ip == pip
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
