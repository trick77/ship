// Package run is the shutdown signal every family program stops on: SIGINT (Ctrl-C) and SIGTERM (docker stop,
// a pod's termination). httpx.Serve uses it for servers; a program without one (an agent's scrape loop, a CLI's
// backfill) uses it directly, so no program carries its own signal list.
package run

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Signals are the shutdown signals: the ones a terminal and a container runtime send.
var Signals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// Context returns a context that ends when parent does or a shutdown signal arrives. stop releases the signal
// handler: after it, a second Ctrl-C kills the process at once, as without a handler.
func Context(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	return signal.NotifyContext(parent, Signals...)
}
