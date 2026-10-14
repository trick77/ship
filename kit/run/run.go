// Package run is the shutdown signal every family program stops on: SIGINT (Ctrl-C) and SIGTERM (docker stop,
// a pod's termination). httpx.Serve uses it for servers; a program without one (an agent's scrape loop, a CLI's
// backfill) uses it directly, so no program carries its own signal list. Sleep is the wait its loops stop on.
package run

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Signals are the shutdown signals: the ones a terminal and a container runtime send.
var Signals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// Context returns a context that ends when parent does or a shutdown signal arrives. The signal handler is released
// shortly after the first stop (an AfterFunc), not at stop: a second Ctrl-C or SIGTERM during a slow stop (a drain, a
// cleanup) kills the process at once, as Go's NotifyContext docs intend for a second Ctrl-C. Tradeoff: a signal
// delivered twice (a process-group Ctrl-C plus a wrapper forwarding it) kills at once too. stop releases the handler
// and the context early; call it when done.
func Context(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	ctx, stop = signal.NotifyContext(parent, Signals...)
	// NotifyContext alone keeps the handler registered, swallowing every further signal, until stop.
	context.AfterFunc(ctx, stop)
	return ctx, stop
}

// Sleep waits d or until ctx ends, whichever is first, and reports whether the full d passed: a loop's
// `for run.Sleep(ctx, every) { … }` stops on shutdown instead of finishing its nap first. d <= 0 returns at once,
// true unless ctx has already ended.
func Sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
