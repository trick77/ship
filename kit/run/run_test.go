package run

import (
	"context"
	"syscall"
	"testing"
	"time"
)

func TestContext_whenSIGTERM_thenDone(t *testing.T) {
	ctx, stop := Context(context.Background())
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not end the context")
	}
}

func TestContext_whenParentEnds_thenDone(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop := Context(parent)
	defer stop()
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent's end did not end the context")
	}
}
