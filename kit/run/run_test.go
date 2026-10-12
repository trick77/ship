package run

import (
	"context"
	"testing"
	"time"
)

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
