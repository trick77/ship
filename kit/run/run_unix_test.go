//go:build unix

package run

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// Unix only: Windows cannot send itself SIGTERM.
func TestContext_whenSIGTERM_thenDone(t *testing.T) {
	ctx, stop := Context(context.Background())
	defer stop()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not end the context")
	}
}
