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

func TestSleep_whenDurationPasses_thenTrue(t *testing.T) {
	if !Sleep(context.Background(), time.Millisecond) {
		t.Fatal("Sleep = false, want true after the full duration")
	}
}

func TestSleep_whenContextEnds_thenFalseAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go cancel()
	start := time.Now()
	if Sleep(ctx, time.Hour) {
		t.Fatal("Sleep = true, want false on a cancelled context")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("Sleep waited %s after the context ended", waited)
	}
}

func TestSleep_whenContextAlreadyEnded_thenFalseOverADueTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 200 {
		if Sleep(ctx, time.Nanosecond) {
			t.Fatalf("call %d: Sleep(ended, 1ns) = true, want false", i)
		}
	}
}

// endsInWait is a context that ends once its Done has been asked for (by the wait's select), with a Done that never
// fires: a cancel that lands as the timer fires, after select has already picked the timer.
type endsInWait struct {
	context.Context
	waited bool
}

func (c *endsInWait) Done() <-chan struct{} {
	c.waited = true
	return nil
}

func (c *endsInWait) Err() error {
	if c.waited {
		return context.Canceled
	}
	return nil
}

func TestSleep_whenContextEndsAsTheTimerFires_thenFalse(t *testing.T) {
	if Sleep(&endsInWait{Context: context.Background()}, time.Millisecond) {
		t.Fatal("Sleep = true, want false: ctx had ended when the timer fired")
	}
}

func TestSleep_whenNoDuration_thenReportsTheContext(t *testing.T) {
	if !Sleep(context.Background(), 0) {
		t.Error("Sleep(live, 0) = false, want true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Sleep(ctx, 0) {
		t.Error("Sleep(ended, 0) = true, want false")
	}
}
