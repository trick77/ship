// Package run is the shutdown signal every family program stops on: SIGINT (Ctrl-C) and SIGTERM (docker stop,
// a pod's termination). httpx.Serve uses it for servers; a program without one (an agent's scrape loop, a CLI's
// backfill) uses it directly, so no program carries its own signal list. Sleep is the wait its loops stop on.
package run

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Signals are the shutdown signals: the ones a terminal and a container runtime send.
var Signals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// Context returns a context that ends when parent does or a shutdown signal arrives. The first SIGINT or SIGTERM is
// a graceful stop (ctx ends, its cause the signal); any later one kills the process — whether the stop came from a
// signal or ctx's parent ended first. Nested Contexts (httpx.Serve inside a program's own) keep that: every layer
// hears the first signal and lets go. Tradeoff: a signal delivered twice (a process-group Ctrl-C plus a wrapper
// forwarding it) kills at once. stop ends ctx and restores the default signal behavior; call it when done.
func Context(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, Signals...)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-ch:
			// A no-op when parent ended first: this signal is then the graceful one, swallowed.
			cancel(signalError{s})
			// The next signal meets the default action, unless another Context still listens.
			signal.Stop(ch)
		case <-done:
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel(nil)
			signal.Stop(ch)
			close(done)
		})
	}
}

// signalError is the cause of a context a signal ended, worded as signal.NotifyContext words its own.
type signalError struct{ sig os.Signal }

func (e signalError) Error() string { return e.sig.String() + " signal received" }

// Is keeps errors.Is(cause, context.Canceled) true, as for NotifyContext's cause.
func (signalError) Is(target error) bool { return target == context.Canceled }

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
