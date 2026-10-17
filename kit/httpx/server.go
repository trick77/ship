package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/trick77/ship/kit/run"
)

// NewServer is an http.Server with the canonical timeouts. ReadHeaderTimeout stops a slowloris holding connections;
// IdleTimeout reaps keep-alives of tabs that are gone. No WriteTimeout and no ReadTimeout: either would cut a
// server-sent event stream or a long download mid-flight. ErrorLog sends what net/http itself reports (a failed TLS
// handshake, a superfluous WriteHeader, an accept error) to slog as an "http_server" line, not unstructured to
// stderr: a panic net/http recovered at error, a failed TLS handshake (scanners, one per probe) at debug, the rest at
// warn. Callers may override any field before Serve.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(serverLog{}, "", 0),
	}
}

// serverLog is NewServer's ErrorLog writer. It resolves slog.Default per line, so a logger set up after NewServer
// still gets them. net/http prefixes its own lines "http: " (HTTP/2 "http2: "), which says nothing the message does
// not.
type serverLog struct{}

func (serverLog) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	// The bundled HTTP/2 server writes to the same ErrorLog, as "http2: ".
	for _, prefix := range []string{"http: ", "http2: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	lvl := slog.LevelWarn
	switch {
	case strings.HasPrefix(msg, "panic serving"):
		lvl = slog.LevelError
	case strings.HasPrefix(msg, "TLS handshake error"):
		lvl = slog.LevelDebug
	}
	slog.Log(context.Background(), lvl, "http_server", "err", msg)
	return len(p), nil
}

// Serve runs srv until ctx ends or the process gets SIGINT/SIGTERM, then drains in-flight requests for up to
// shutdownTimeout. A clean shutdown returns nil; a listener that cannot bind returns its error at once. A drain that
// outlasts shutdownTimeout returns context.DeadlineExceeded after Close cuts the remaining connections; handlers it
// cut may still be running when Serve returns, so a caller's cleanup must tolerate that.
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
		if errors.Is(err, context.DeadlineExceeded) {
			// Overrun: the handlers still running get their connections closed and their request contexts ended.
			_ = srv.Close()
		}
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
