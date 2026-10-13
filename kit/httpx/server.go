package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
// shutdownTimeout. A clean shutdown returns nil; a listener that cannot bind returns its error at once. A drain
// that outlasts shutdownTimeout closes the server, cutting off the connections still open, and returns
// context.DeadlineExceeded: whatever the caller closes next is no longer read through them. Neither Shutdown nor
// Close waits for a handler to return or for a hijacked connection (a WebSocket); Main also waits for those.
func Serve(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) error {
	ctx, stop := run.Context(ctx)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// A fresh context: the one that just ended would cancel the drain before it starts.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		// Overrun: the handlers still running get their connections closed and their request contexts ended.
		_ = srv.Close()
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// LocalURL turns a listen address into a URL on this host: ":8080" and "0.0.0.0:8080" become
// "http://127.0.0.1:8080"+path. A wildcard bind is not an address a client can dial.
func LocalURL(addr, path string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "80"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://" + net.JoinHostPort(host, port) + path
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
