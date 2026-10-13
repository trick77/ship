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

// Context returns a context that ends when parent does or a shutdown signal arrives. stop releases the signal
// handler: after it, a second Ctrl-C kills the process at once, as without a handler.
func Context(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	return signal.NotifyContext(parent, Signals...)
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
