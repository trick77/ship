package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// A misconfigured throttle fails the boot, as a misconfigured Limiter does.
func TestNewThrottle_whenMisconfigured_thenPanics(t *testing.T) {
	for name, args := range map[string][2]time.Duration{
		"zero gap":         {0, time.Second},
		"negative gap":     {-time.Second, time.Second},
		"negative maxWait": {time.Second, -time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewThrottle(%v, %v) did not panic", args[0], args[1])
				}
			}()
			NewThrottle(args[0], args[1])
		})
	}
	// maxWait 0 is a valid "never queue": the second attempt within gap is refused outright.
	th := NewThrottle(time.Second, 0)
	if err := th.Wait(context.Background(), "k"); err != nil {
		t.Fatalf("first Wait = %v", err)
	}
	if err := th.Wait(context.Background(), "k"); !errors.Is(err, ErrThrottled) {
		t.Errorf("second Wait within gap, maxWait 0 = %v, want ErrThrottled", err)
	}
}

func TestThrottle(t *testing.T) {
	th := NewThrottle(500*time.Millisecond, time.Second)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	waits := []time.Duration{}
	if got := th.RetryAfter("a"); got != 0 {
		t.Errorf("RetryAfter of an unknown key = %s, want 0", got)
	}
	for range 3 {
		_, d, ok := th.turn("a")
		if !ok {
			t.Fatal("refused too early")
		}
		waits = append(waits, d)
	}
	if waits[0] != 0 || waits[1] != 500*time.Millisecond || waits[2] != time.Second {
		t.Errorf("waits = %v", waits)
	}
	if _, _, ok := th.turn("a"); ok {
		t.Error("queue past maxWait accepted")
	}
	// Next free slot at +1.5s, maxWait 1s: let in again in 500ms.
	if got := th.RetryAfter("a"); got != 500*time.Millisecond {
		t.Errorf("RetryAfter of a refused key = %s, want 500ms", got)
	}
	now = now.Add(500 * time.Millisecond)
	if got := th.RetryAfter("a"); got != 0 {
		t.Errorf("RetryAfter once due = %s, want 0", got)
	}
	if _, _, ok := th.turn("a"); !ok {
		t.Error("refused once RetryAfter said 0")
	}
	if _, d, ok := th.turn("b"); !ok || d != 0 {
		t.Error("one key's flood throttled another")
	}
	// Stale entries are swept once the map is large.
	for i := range 1100 {
		th.next[string(rune(i+1000))] = now.Add(-time.Hour)
	}
	now = now.Add(time.Hour)
	th.turn("c")
	if len(th.next) > 10 {
		t.Errorf("stale entries kept: %d", len(th.next))
	}
}

// Only the latest booking is given back: with another queued behind it, moving next would hand that slot out twice.
func TestThrottle_whenLaterBookingQueued_thenReleaseKeepsIt(t *testing.T) {
	th := NewThrottle(time.Second, time.Hour)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	th.turn("k")
	a, _, _ := th.turn("k")
	b, _, _ := th.turn("k")
	th.release("k", a)
	if got := th.next["k"]; !got.Equal(b.Add(time.Second)) {
		t.Errorf("next = %v after releasing an earlier booking, want %v", got, b.Add(time.Second))
	}
	th.release("k", b)
	if got := th.next["k"]; !got.Equal(b) {
		t.Errorf("next = %v after releasing the latest booking, want its slot %v", got, b)
	}
	th.release("gone", a) // a key swept meanwhile is not recreated
	if _, ok := th.next["gone"]; ok {
		t.Error("release booked a key it never had")
	}
}

// Live entries are not rescanned on every call: the next sweep waits until the map has doubled or an interval has
// passed.
func TestThrottle_whenEntriesLive_thenSweepAmortized(t *testing.T) {
	th := NewThrottle(time.Minute, time.Minute)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	for i := range 1025 {
		th.turn(strconv.Itoa(i))
	}
	th.turn("first sweep") // 1025 > minPrune: sweeps, keeps all 1025 live
	if th.pruneAt != 2050 || !th.swept.Equal(now) {
		t.Fatalf("pruneAt %d swept %v after the first sweep, want 2050 at %v", th.pruneAt, th.swept, now)
	}
	now = now.Add(90 * time.Second) // 1025 stale, interval of 2m not yet over
	for i := range 1000 {
		th.turn("live" + strconv.Itoa(i))
	}
	if n := len(th.next); n != 2026 {
		t.Fatalf("len = %d, want 2026: no sweep before the map doubles or the interval ends", n)
	}
	th.turn("doubling") // 2026, then 2027 entries: still under 2050
	for i := range 30 {
		th.turn("more" + strconv.Itoa(i))
	}
	if _, ok := th.next["0"]; ok {
		t.Error("map doubled past pruneAt without a sweep")
	}
	if n := len(th.next); n != 1031 {
		t.Errorf("len = %d after the doubling sweep, want the 1031 live keys", n)
	}
}

// A burst's keys go once it is over, without waiting for the map to double again.
func TestThrottle_whenBurstGoesQuiet_thenSweptAfterInterval(t *testing.T) {
	th := NewThrottle(time.Minute, time.Minute)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	for i := range 5000 {
		th.turn(strconv.Itoa(i))
	}
	now = now.Add(2 * time.Minute)
	th.turn("after")
	if n := len(th.next); n != 1 {
		t.Errorf("len = %d one interval after the burst, want only the new key", n)
	}
}

func TestThrottleWait(t *testing.T) {
	th := NewThrottle(time.Hour, time.Hour)
	if err := th.Wait(context.Background(), "k"); err != nil {
		t.Fatalf("first Wait = %v", err)
	}
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	if err := th.Wait(gone, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait with a cancelled context = %v", err)
	}
	if got := th.RetryAfter("k"); got != 0 {
		t.Errorf("a Wait cancelled before asking booked a turn: RetryAfter = %s", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := th.Wait(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timed-out Wait = %v", err)
	}
	// The cancelled attempt gave its turn back: k's next one is queued at +1h again, not refused at +2h.
	queued, cancelQueued := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- th.Wait(queued, "k") }()
	for deadline := time.Now().Add(5 * time.Second); th.RetryAfter("k") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the queued Wait never booked its turn")
		}
		time.Sleep(time.Millisecond)
	}
	if err := th.Wait(context.Background(), "k"); !errors.Is(err, ErrThrottled) {
		t.Errorf("over-queued Wait = %v", err)
	}
	cancelQueued()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("queued Wait after cancel = %v", err)
	}
	quick := NewThrottle(time.Millisecond, time.Second)
	_ = quick.Wait(context.Background(), "k")
	if err := quick.Wait(context.Background(), "k"); err != nil {
		t.Errorf("short Wait = %v", err)
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

// A caller whose ctx ends as its slot comes due gave up: Wait reports ctx's error and gives the slot back.
func TestThrottleWait_whenContextEndsAsTheSlotComesDue_thenGivesUp(t *testing.T) {
	th := NewThrottle(time.Millisecond, time.Second)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	if err := th.Wait(context.Background(), "k"); err != nil {
		t.Fatalf("first Wait = %v", err)
	}
	if err := th.Wait(&endsInWait{Context: context.Background()}, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	if got, want := th.next["k"], now.Add(time.Millisecond); !got.Equal(want) {
		t.Errorf("next = %s, want %s: the abandoned slot was not given back", got, want)
	}
}

// The refusal carries the wait decided under its lock: a later RetryAfter reads the queue anew.
func TestThrottleWait_whenRefused_thenErrorCarriesWait(t *testing.T) {
	th := NewThrottle(500*time.Millisecond, time.Second)
	now := time.Unix(1000, 0)
	th.now = func() time.Time { return now }
	for range 3 {
		if _, _, ok := th.turn("a"); !ok {
			t.Fatal("refused too early")
		}
	}
	err := th.Wait(context.Background(), "a")
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("Wait = %v, want ErrThrottled", err)
	}
	var te *ThrottledError
	if !errors.As(err, &te) {
		t.Fatalf("Wait = %T, want *ThrottledError", err)
	}
	// Next free slot at +1.5s, maxWait 1s: let in again in 500ms, what RetryAfter reads at the same instant.
	if te.RetryAfter != 500*time.Millisecond || te.RetryAfter != th.RetryAfter("a") {
		t.Errorf("refusal's RetryAfter = %s, RetryAfter = %s, want both 500ms", te.RetryAfter, th.RetryAfter("a"))
	}
	if want := "too many attempts, retry after 500ms"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	now = now.Add(200 * time.Millisecond)
	if te.RetryAfter != 500*time.Millisecond || th.RetryAfter("a") != 300*time.Millisecond {
		t.Errorf("after 200ms: refusal's %s (want 500ms kept), RetryAfter %s (want 300ms)", te.RetryAfter, th.RetryAfter("a"))
	}
	if errors.Is(err, context.Canceled) {
		t.Error("the refusal matched context.Canceled")
	}
	wrapped := fmt.Errorf("login: %w", err)
	if !errors.Is(wrapped, ErrThrottled) || !errors.As(wrapped, &te) {
		t.Errorf("wrapped refusal lost ErrThrottled or its type: %v", wrapped)
	}
}
