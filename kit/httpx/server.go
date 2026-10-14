package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ship/kit/run"
)

// NewServer is an http.Server with the canonical timeouts. ReadHeaderTimeout stops a slowloris holding connections;
// IdleTimeout reaps keep-alives of tabs that are gone. No WriteTimeout and no ReadTimeout: either would cut a
// server-sent event stream or a long download mid-flight. Callers may override any field before Serve.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Serve runs srv until ctx ends or the process gets SIGINT/SIGTERM, then drains in-flight requests for up to
// shutdownTimeout. A clean shutdown returns nil; a listener that cannot bind returns its error at once. Once the stop
// is in, Serve releases its signal handler: a second Ctrl-C or SIGTERM during the drain kills the process at once.
// A drain that outlasts shutdownTimeout closes the server, cutting off the connections still open, and returns
// context.DeadlineExceeded: whatever the caller closes next is no longer read through them. Any other Shutdown error
// (a listener that fails to close after a clean drain) is returned as is. A server that fails on its own (its
// accept loop errs) is closed too before its error returns: kept-alive connections must not go on serving. A
// Shutdown the program calls itself owns its drain; Serve returns nil when it begins.
func Serve(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) error {
	ctx, stop := run.Context(ctx)
	defer stop()
	// The handler goes as soon as the stop is in, not at return: a second signal must not wait out the drain.
	// NotifyContext alone keeps it registered, swallowing every further signal, until stop.
	context.AfterFunc(ctx, stop)
	_, _, err := serve(ctx, srv, shutdownTimeout)
	return err
}

// listen opens serve's listener; a var only for the tests of a failing one. srv.Serve closes it.
var listen = net.Listen

// serve is Serve without the signal handling (ctx is taken as given: Serve and Main bind the signals themselves) and
// also returns when the stop came in (the return time otherwise) and whether the server was shut down rather than
// failed: Main counts its whole stop budget from stoppedAt and tells a failure from a stop.
func serve(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) (stoppedAt time.Time, shutdown bool, err error) {
	addr := srv.Addr
	if addr == "" {
		addr = ":http" // what ListenAndServe listens on
	}
	ln, err := listen("tcp", addr)
	if err != nil {
		return time.Now(), false, err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err = <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			// The program's own Shutdown: its drain, not a second one here (its RegisterOnShutdown hooks would run twice).
			return time.Now(), true, nil
		}
		// Failed on its own: the connections it accepted would go on dispatching requests.
		_ = srv.Close()
		return time.Now(), false, err
	case <-ctx.Done():
	}
	stoppedAt = time.Now()
	// A fresh context: the one that just ended would cancel the drain before it starts.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Overrun: the handlers still running get their connections closed and their request contexts ended.
			_ = srv.Close()
		}
		// Otherwise the drain finished, and the error is only a listener that failed to close.
		return stoppedAt, true, err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return stoppedAt, true, err
	}
	return stoppedAt, true, nil
}

// splitAddr splits a listen address into its host, normalized, and its port as a number (":http" is 80, ":08080"
// is 8080, as Listen reads them). The host is lowercased, an IP in its canonical form, and all interfaces (none,
// 0.0.0.0, :: in any spelling) is "".
func splitAddr(addr string) (host string, port int, err error) {
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	if port, err = net.LookupPort("tcp", p); err != nil {
		return "", 0, err
	}
	host = strings.ToLower(host)
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
		if ip.IsUnspecified() {
			host = ""
		}
	}
	return host, port, nil
}

// LocalURL turns a listen address into a URL on this host: ":8080" and "0.0.0.0:8080" become
// "http://127.0.0.1:8080"+path. A wildcard bind is not an address a client can dial. A named port (":http") becomes
// its number, as Listen reads it; an addr without a usable port is a host on port 80.
func LocalURL(addr, path string) string {
	host, port, err := splitAddr(addr)
	if err != nil {
		host, port, _ = splitAddr(net.JoinHostPort(addr, "80"))
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + path
}

// Healthcheck probes url and returns the process exit code: 0 on a 2xx, 1 otherwise. It is the container probe of a
// distroless image (no shell, no curl): the binary re-executes itself with -healthcheck. Redirects are not followed;
// a probe endpoint that redirects is misrouted. A failure says why on stderr, where the runtime's health log
// (docker/podman inspect) keeps it.
func Healthcheck(url string) int {
	return healthcheck(url, os.Stderr)
}

func healthcheck(url string, w io.Writer) int {
	c := http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get(url) //nolint:gosec // the process's own listener, from its own config
	if err != nil {
		_, _ = fmt.Fprintf(w, "healthcheck: %v\n", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The status alone: url may carry userinfo, and the health log keeps what this prints.
		_, _ = fmt.Fprintf(w, "healthcheck: %s\n", resp.Status)
		return 1
	}
	return 0
}
