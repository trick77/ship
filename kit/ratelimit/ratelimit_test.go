package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// clock is a settable Now.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func limiter(c *clock, rate, burst float64) *Limiter {
	return New(Options{Rate: rate, Burst: burst, Now: c.now})
}

func TestAllow_whenBurstSpent_thenRefusedUntilRefill(t *testing.T) {
	c := newClock()
	l := limiter(c, 1, 3)
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("request %d refused inside the burst", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("fourth request allowed past a burst of 3")
	}
	if !l.Allow("b") {
		t.Fatal("another key shares the first one's bucket")
	}
	c.advance(time.Second)
	if !l.Allow("a") {
		t.Fatal("one second at rate 1 did not refill a token")
	}
}

func TestAllow_whenIdle_thenRefillStopsAtBurst(t *testing.T) {
	c := newClock()
	l := limiter(c, 1, 2)
	l.Allow("a")
	c.advance(time.Hour)
	allowed := 0
	for l.Allow("a") {
		allowed++
	}
	if allowed != 2 {
		t.Errorf("allowed %d after an hour idle, want the burst of 2", allowed)
	}
}

func TestCharge_whenKnockingWhileBlocked_thenDebtDeepensTheWait(t *testing.T) {
	c := newClock()
	l := limiter(c, 0.5, 2)
	l.Charge("a", 2)
	if l.Permitted("a") {
		t.Fatal("permitted with an empty bucket")
	}
	first := l.RetryAfter("a")
	if first != 2*time.Second {
		t.Fatalf("RetryAfter = %s, want 2s for one token at 0.5/s", first)
	}
	l.Charge("a", 100) // floored at -Burst
	if got := l.RetryAfter("a"); got != 6*time.Second {
		t.Errorf("RetryAfter in full debt = %s, want 6s (from -2 to 1 at 0.5/s)", got)
	}
}

func TestRetryAfter(t *testing.T) {
	c := newClock()
	l := limiter(c, 1, 1)
	if got := l.RetryAfter("unknown"); got != 0 {
		t.Errorf("unknown key = %s, want 0", got)
	}
	l.Allow("a")
	if got := l.RetryAfter("a"); got != time.Second {
		t.Errorf("spent = %s, want 1s", got)
	}
	c.advance(time.Second)
	if got := l.RetryAfter("a"); got != 0 {
		t.Errorf("refilled = %s, want 0", got)
	}
}

func TestPermitted_doesNotSpend(t *testing.T) {
	l := limiter(newClock(), 1, 1)
	for range 3 {
		if !l.Permitted("a") {
			t.Fatal("Permitted spent the token")
		}
	}
}

// A full bucket is a fresh one, so pruning it forgives nothing; a bucket in debt stays, or pausing for the prune
// would clear a scanner's record.
func TestPrune_whenFullBucketsIdle_thenDroppedButDebtKept(t *testing.T) {
	c := newClock()
	l := limiter(c, 1, 100)
	l.Allow("visitor")                  // 99, full again within the minute
	l.Charge("scanner", 1000)           // floored at -100, still -39 a minute on
	c.advance(pruneEvery + time.Second) // visitor refilled to full; scanner still deep in debt
	l.Allow("newcomer")                 // a new key triggers the prune
	if l.Len() != 2 {
		t.Fatalf("Len = %d, want scanner and newcomer", l.Len())
	}
	if l.Permitted("scanner") {
		t.Error("the prune forgave the scanner's debt")
	}
}

func TestPrune_whenMapFull_thenBoundHolds(t *testing.T) {
	c := newClock()
	l := New(Options{Rate: 1e-9, Burst: 1, MaxKeys: 100, Now: c.now})
	for i := range 1000 {
		c.advance(time.Millisecond)
		l.Allow(strconv.Itoa(i)) // spent, never refilled: nothing is full, so only the bound evicts
	}
	if n := l.Len(); n > 100 {
		t.Errorf("Len = %d, want at most MaxKeys 100", n)
	}
	if l.Permitted("999") {
		t.Error("the newest key lost its spent bucket; the longest idle should go first")
	}
}

// A misconfigured limit fails the boot: a forgotten Burst would refuse everyone, a NaN rate admit everyone.
func TestNew_whenMisconfigured_thenPanics(t *testing.T) {
	for name, o := range map[string]Options{
		"zero rate":      {Rate: 0, Burst: 1},
		"negative rate":  {Rate: -1, Burst: 1},
		"NaN rate":       {Rate: math.NaN(), Burst: 1},
		"infinite rate":  {Rate: math.Inf(1), Burst: 1},
		"burst left out": {Rate: 1},
		"burst below 1":  {Rate: 1, Burst: 0.5},
		"negative burst": {Rate: 1, Burst: -5},
		"NaN burst":      {Rate: 1, Burst: math.NaN()},
		"infinite burst": {Rate: 1, Burst: math.Inf(1)},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("no panic for %+v", o)
				}
			}()
			New(o)
		})
	}
}

func TestCharge_whenNegative_thenRefundsNoPastBurst(t *testing.T) {
	l := limiter(newClock(), 1, 3)
	l.Charge("a", -100)
	allowed := 0
	for l.Allow("a") {
		allowed++
	}
	if allowed != 3 {
		t.Errorf("allowed %d after a refund of 100, want the burst of 3", allowed)
	}
}

// The bound is reachable by anyone with fresh keys. Evicting by age alone would let a blocked scanner spray new
// keys until its own record is the longest idle and goes; blocked buckets go last.
func TestPrune_whenMapFull_thenBlockedGoesLast(t *testing.T) {
	c := newClock()
	l := New(Options{Rate: 1e-9, Burst: 2, MaxKeys: 50, Now: c.now})
	l.Charge("scanner", 2) // oldest, blocked
	for i := range 500 {
		c.advance(time.Millisecond)
		l.Allow(strconv.Itoa(i)) // part-spent, never full
	}
	if l.Permitted("scanner") || l.RetryAfter("scanner") == 0 {
		t.Error("spraying fresh keys evicted the blocked scanner")
	}
}

func TestNew_defaults(t *testing.T) {
	l := New(Options{Rate: 1, Burst: 1})
	if l.o.MaxKeys != DefaultMaxKeys || l.o.Now == nil {
		t.Errorf("defaults not applied: %+v", l.o)
	}
	if !l.Allow("a") {
		t.Error("real clock: first request refused")
	}
}

func TestConcurrentUse(t *testing.T) {
	l := New(Options{Rate: 1e-9, Burst: 100})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Go(func() {
			if l.Allow("a") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("allowed %d of 200 concurrent requests, want the burst of 100", allowed)
	}
}

// Take gates and charges under one lock: parallel requests cannot all pass before any is charged.
func TestTake_whenConcurrent_thenOnlyTheBurstPasses(t *testing.T) {
	l := New(Options{Rate: 1e-9, Burst: 100})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 200 {
		wg.Go(func() {
			if ok, _ := l.Take("a", 1, 1); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("allowed %d of 200 concurrent Takes, want the burst of 100", allowed)
	}
}

func TestTake_whenRefused_thenPenaltyDeepensTheWait(t *testing.T) {
	c := newClock()
	l := limiter(c, 0.5, 2)
	if ok, wait := l.Take("a", 2, 1); !ok || wait != 0 {
		t.Fatalf("first Take = %v %s, want allowed", ok, wait)
	}
	if ok, wait := l.Take("a", 1, 0); ok || wait != 2*time.Second {
		t.Errorf("Take without penalty = %v %s, want refused, 2s", ok, wait)
	}
	if ok, wait := l.Take("a", 1, 1); ok || wait != 4*time.Second {
		t.Errorf("Take with penalty = %v %s, want refused, 4s (from -1 to 1 at 0.5/s)", ok, wait)
	}
	if got := l.RetryAfter("a"); got != 4*time.Second {
		t.Errorf("RetryAfter = %s, want the penalised 4s", got)
	}
}

// The documented 404 pattern: reserve, refund a free outcome.
func TestTake_whenRefunded_thenOnlyCostlyOutcomesCount(t *testing.T) {
	l := limiter(newClock(), 1e-9, 2)
	for range 10 {
		if ok, _ := l.Take("a", 1, 1); !ok {
			t.Fatal("free outcomes ran the bucket dry")
		}
		l.Charge("a", -1)
	}
	l.Take("a", 1, 1)
	l.Take("a", 1, 1)
	if ok, _ := l.Take("a", 1, 1); ok {
		t.Error("third 404 allowed past a burst of 2")
	}
}

func TestMiddleware(t *testing.T) {
	c := newClock()
	l := limiter(c, 0.1, 1)
	h := l.Middleware(ClientKey(1))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	get := func(xff string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if code := get("2001:db8:1:2::1").Code; code != http.StatusNoContent {
		t.Fatalf("first = %d, want through", code)
	}
	// Another address in the same /64 is the same caller.
	rec := get("2001:db8:1:2:ffff::9")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" {
		t.Errorf("same /64 = %d Retry-After %q, want 429 and 10 (one token at 0.1/s)", rec.Code, rec.Header().Get("Retry-After"))
	}
	if code := get("2001:db8:1:3::1").Code; code != http.StatusNoContent {
		t.Errorf("next /64 = %d, want its own bucket", code)
	}
}

func TestKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7:51234":                      "203.0.113.7",
		"[2001:db8:1:2::9]:443":                  "2001:db8:1:2::/64",
		"[2001:db8:1:2::9]":                      "2001:db8:1:2::/64",
		"203.0.113.7":                            "203.0.113.7",
		"::ffff:203.0.113.7":                     "203.0.113.7",
		"2001:db8:1234:5678:abcd:ef01:2345:6789": "2001:db8:1234:5678::/64",
		"2001:db8:1234:5678::1":                  "2001:db8:1234:5678::/64",
		"fe80::1%eth0":                           "fe80::/64",
		"not-an-address":                         "not-an-address",
		"":                                       "",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

func ExampleKey() {
	fmt.Println(Key("2001:db8:1:2:3:4:5:6"))
	// Output: 2001:db8:1:2::/64
}
