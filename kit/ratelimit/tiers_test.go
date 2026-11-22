package ratelimit

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// from is a request from ip, behind one trusted proxy.
func from(ip string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/events", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", ip)
	return r
}

// per64And48 is the documented pair: narrow per /64, wide per /48, on one clock.
func per64And48(c *clock, narrow, wide Options) (n, w *Limiter, t *Tiers) {
	narrow.Now, wide.Now = c.now, c.now
	n, w = New(narrow), New(wide)
	return n, w, NewTiers(Tier{Limiter: n, Key: ClientKey(1)}, Tier{Limiter: w, Key: ClientPrefixKey(1, 32, 48)})
}

func TestPrefixKey(t *testing.T) {
	for _, tc := range []struct {
		v4, v6  int
		in, out string
	}{
		{24, 48, "203.0.113.7", "203.0.113.0/24"},
		{24, 48, "203.0.113.7:51234", "203.0.113.0/24"},
		{24, 48, "::ffff:203.0.113.7", "203.0.113.0/24"},
		{24, 48, "[::ffff:203.0.113.7]:443", "203.0.113.0/24"},
		{32, 48, "203.0.113.7", "203.0.113.7"},
		{16, 56, "203.0.113.7", "203.0.0.0/16"},
		{24, 48, "2001:db8:1:2:3:4:5:6", "2001:db8:1::/48"},
		{24, 48, "[2001:db8:1:ff::9]:443", "2001:db8:1::/48"},
		{24, 48, "[2001:db8:1:ff::9]", "2001:db8:1::/48"},
		{24, 56, "2001:db8:1:2ff::9", "2001:db8:1:200::/56"},
		{24, 48, "fe80::1%eth0", "fe80::/48"},
		{24, 48, "[fe80::1%eth0]:443", "fe80::/48"},
		{24, 48, "not-an-address", "not-an-address"},
		{24, 48, "", ""},
	} {
		if got := PrefixKey(tc.v4, tc.v6)(tc.in); got != tc.out {
			t.Errorf("PrefixKey(%d, %d)(%q) = %q, want %q", tc.v4, tc.v6, tc.in, got, tc.out)
		}
	}
}

// PrefixKey at a host's v4 and a /64 is Key, byte for byte.
func TestPrefixKey_whenAddressAnd64_thenKey(t *testing.T) {
	key := PrefixKey(32, 64)
	for _, in := range []string{
		"203.0.113.7:51234", "[2001:db8:1:2::9]:443", "[2001:db8:1:2::9]", "203.0.113.7", "::ffff:203.0.113.7",
		"2001:db8:1234:5678:abcd:ef01:2345:6789", "fe80::1%eth0", "not-an-address", "",
	} {
		if got, want := key(in), Key(in); got != want {
			t.Errorf("PrefixKey(32, 64)(%q) = %q, Key = %q", in, got, want)
		}
	}
}

// /0 is one bucket for everybody, a v6 past /64 one the caller rotates out of: both fail the boot.
func TestPrefixKey_whenBitsOutOfRange_thenPanics(t *testing.T) {
	for _, bits := range [][2]int{{0, 48}, {-1, 48}, {33, 48}, {24, 0}, {24, 65}, {24, 128}} {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("no panic for v4 /%d, v6 /%d", bits[0], bits[1])
				}
			}()
			ClientPrefixKey(1, bits[0], bits[1])
		})
	}
}

func TestClientPrefixKey(t *testing.T) {
	if got := ClientPrefixKey(1, 24, 48)(from("2001:db8:1:2::1")); got != "2001:db8:1::/48" {
		t.Errorf("key = %q, want the caller's /48", got)
	}
}

// The point of the wide tier: a caller holding a /48 rotates through /64s, each a fresh narrow bucket.
func TestTiers_whenCallerRotates64s_then48Refuses(t *testing.T) {
	c := newClock()
	_, _, tiers := per64And48(c, Options{Rate: 1e-9, Burst: 2}, Options{Rate: 1e-9, Burst: 10})
	admitted := 0
	for i := range 100 {
		if ok, _ := tiers.Take(from(fmt.Sprintf("2001:db8:1:%x::1", i)), 1); ok {
			admitted++
		}
	}
	if admitted != 10 {
		t.Errorf("admitted %d across 100 /64s of one /48, want the /48's burst of 10", admitted)
	}
	if ok, _ := tiers.Take(from("2001:db8:2::1"), 1); !ok {
		t.Error("the next /48 shares the first one's bucket")
	}
}

// A refusal by the wide tier leaves the narrow one as it was: no charge, no bucket.
func TestTiers_whenWideRefuses_thenNarrowUncharged(t *testing.T) {
	c := newClock()
	narrow, wide, tiers := per64And48(c, Options{Rate: 1e-9, Burst: 3}, Options{Rate: 1e-9, Burst: 1})
	if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 1); !ok {
		t.Fatal("first request refused")
	}
	if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 1); ok {
		t.Fatal("the /48's burst of 1 admitted a second request")
	}
	if ok, _ := tiers.Take(from("2001:db8:1:2::1"), 1); ok {
		t.Fatal("a fresh /64 got past the spent /48")
	}
	if n := narrow.Len(); n != 1 {
		t.Errorf("narrow Len = %d, want 1: a refused /64 got a bucket", n)
	}
	allowed := 0
	for narrow.Allow("2001:db8:1:1::/64") {
		allowed++
	}
	if allowed != 2 {
		t.Errorf("narrow /64 holds %d tokens, want 2: the refusals were charged", allowed)
	}
	if ok, _ := wide.Take("2001:db8:1::/48", 0, 0); ok {
		t.Error("the wide tier admits after refusing")
	}
}

// tokens counts what key's bucket holds by spending it.
func tokens(l *Limiter, key string) int {
	n := 0
	for l.Allow(key) {
		n++
	}
	return n
}

// The gate is exact: a batch is admitted only when every tier holds all of it, never into debt.
func TestTiers_whenCostAboveBalance_thenRefusedUncharged(t *testing.T) {
	c := newClock()
	narrow, wide, tiers := per64And48(c, Options{Rate: 1, Burst: 10}, Options{Rate: 10, Burst: 100})
	if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 4); !ok {
		t.Fatal("batch of 4 refused by full buckets")
	}
	if ok, wait := tiers.Take(from("2001:db8:1:1::1"), 8); ok || wait != 2*time.Second {
		t.Fatalf("batch of 8 on 6 tokens = %v %s, want refused, 2s (from 6 to 8 at 1/s)", ok, wait)
	}
	if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 6); !ok {
		t.Fatal("batch of 6 on 6 tokens refused: the refused 8 was charged")
	}
	if n := tokens(narrow, "2001:db8:1:1::/64"); n != 0 {
		t.Errorf("narrow holds %d tokens, want 0 after 4 + 6", n)
	}
	if n := tokens(wide, "2001:db8:1::/48"); n != 90 {
		t.Errorf("wide holds %d tokens, want 90 after 4 + 6", n)
	}
}

// A wait past time.Duration's range is the longest Duration, never a wrapped negative one.
func TestTiers_whenWaitOverflows_thenLongestDuration(t *testing.T) {
	_, _, tiers := per64And48(newClock(), Options{Rate: 1e-9, Burst: 100}, Options{Rate: 1e-9, Burst: 100})
	tiers.Take(from("2001:db8:1:1::1"), 100)
	if ok, wait := tiers.Take(from("2001:db8:1:1::1"), 100); ok || wait != math.MaxInt64 {
		t.Errorf("Take = %v %s, want refused, the longest Duration", ok, wait)
	}
}

// A batch above MaxCost fits no wait: refused with 0 (the 413), metered as one token so sending it is not free.
func TestTiers_whenAboveMaxCost_thenRefusedWithoutWait(t *testing.T) {
	c := newClock()
	narrow, wide, tiers := per64And48(c, Options{Rate: 1, Burst: 10}, Options{Rate: 10, Burst: 100})
	if got := tiers.MaxCost(); got != 10 {
		t.Fatalf("MaxCost = %v, want the smallest Burst, 10", got)
	}
	if ok, wait := tiers.Take(from("2001:db8:1:1::1"), 11); ok || wait != 0 {
		t.Fatalf("batch of 11 = %v %s, want refused without a wait", ok, wait)
	}
	if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 9); !ok {
		t.Fatal("batch of 9 on the 9 tokens the oversized refusal left refused")
	}
	for _, cost := range []float64{11, math.Inf(1)} {
		if ok, wait := tiers.Take(from("2001:db8:1:1::1"), cost); ok || wait != time.Second {
			t.Errorf("batch of %v on an empty bucket = %v %s, want the 429's 1s for one token", cost, ok, wait)
		}
	}
	if n := tokens(wide, "2001:db8:1::/48"); n != 90 {
		t.Errorf("wide holds %d tokens, want 90 after 1 + 9", n)
	}
	if n := tokens(narrow, "2001:db8:1:1::/64"); n != 0 {
		t.Errorf("narrow holds %d tokens, want 0", n)
	}
}

// An hour of 10,000-event batches every second from one /64 admits no event, and leaves its /48 neighbours a full
// tier; an hour of MaxCost batches admits no more than Burst plus the refill.
func TestTiers_whenBatchesForAnHour_thenNothingUnpaid(t *testing.T) {
	c := newClock()
	_, _, tiers := per64And48(c, Options{Rate: 1, Burst: 10}, Options{Rate: 10, Burst: 100})
	admitted := 0.0
	for range 3600 {
		if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 10_000); ok {
			admitted += 10_000
		}
		c.advance(time.Second)
	}
	if admitted != 0 {
		t.Errorf("admitted %v events of oversized batches", admitted)
	}
	if ok, _ := tiers.Take(from("2001:db8:1:2::1"), 10); !ok {
		t.Error("a neighbouring /64 refused: the oversized batches ran the /48 into debt")
	}

	c = newClock()
	_, _, tiers = per64And48(c, Options{Rate: 1, Burst: 10}, Options{Rate: 10, Burst: 100})
	admitted = 0
	for range 36_000 {
		if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 10); ok {
			admitted += 10
		}
		c.advance(100 * time.Millisecond)
	}
	if admitted > 10+3600 || admitted < 3500 {
		t.Errorf("admitted %v events in an hour, want at most Burst 10 + 3600 refilled, and close to it", admitted)
	}
}

// Every call costs at least one token: an empty batch, a negative or NaN cost neither passes free nor refunds.
func TestTiers_whenCostBelowOne_thenOneToken(t *testing.T) {
	for _, cost := range []float64{0, 0.5, -100, math.NaN()} {
		t.Run(fmt.Sprint(cost), func(t *testing.T) {
			narrow, _, tiers := per64And48(newClock(), Options{Rate: 1e-9, Burst: 1}, Options{Rate: 1e-9, Burst: 1})
			admitted := 0
			for range 10_000 {
				if ok, _ := tiers.Take(from("2001:db8:1:1::1"), cost); ok {
					admitted++
				}
			}
			if admitted != 1 {
				t.Errorf("admitted %d of 10,000 at cost %v, want the burst of 1", admitted, cost)
			}
			if narrow.Permitted("2001:db8:1:1::/64") {
				t.Errorf("cost %v refunded the bucket", cost)
			}
		})
	}
}

// The zero Tiers has no tier and would admit everything; it panics instead.
func TestTiers_whenZero_thenPanics(t *testing.T) {
	var zero Tiers
	for name, use := range map[string]func(){
		"Take":       func() { zero.Take(from("203.0.113.7"), 1) },
		"MaxCost":    func() { zero.MaxCost() },
		"Middleware": func() { zero.Middleware(http.NotFoundHandler()) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("zero Tiers: %s did not panic", name)
				}
			}()
			use()
		})
	}
}

// Retry-After is the longest wait among the refusing tiers, not the first one's.
func TestTiers_whenSeveralRefuse_thenLongestWait(t *testing.T) {
	c := newClock()
	narrow, wide, tiers := per64And48(c, Options{Rate: 1, Burst: 1}, Options{Rate: 0.125, Burst: 1})
	narrow.Charge("2001:db8:1:1::/64", 1) // 1s to a token
	wide.Charge("2001:db8:1::/48", 1)     // 8s
	if ok, wait := tiers.Take(from("2001:db8:1:1::1"), 1); ok || wait != 8*time.Second {
		t.Errorf("both refusing = %v %s, want refused, the /48's 8s", ok, wait)
	}
	c.advance(time.Second) // narrow refilled, wide not
	if ok, wait := tiers.Take(from("2001:db8:1:1::1"), 1); ok || wait != 7*time.Second {
		t.Errorf("wide refusing = %v %s, want refused, 7s", ok, wait)
	}
	if !narrow.Permitted("2001:db8:1:1::/64") {
		t.Error("the refusal charged the admitting narrow tier")
	}
}

func TestTiers_Middleware(t *testing.T) {
	c := newClock()
	_, _, tiers := per64And48(c, Options{Rate: 0.1, Burst: 1}, Options{Rate: 0.5, Burst: 2})
	h := tiers.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	post := func(ip string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, from(ip))
		return rec
	}
	if code := post("2001:db8:1:1::1").Code; code != http.StatusNoContent {
		t.Fatalf("first = %d, want through", code)
	}
	rec := post("2001:db8:1:1::2")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" {
		t.Errorf("same /64 = %d Retry-After %q, want 429 and 10", rec.Code, rec.Header().Get("Retry-After"))
	}
	if code := post("2001:db8:1:2::1").Code; code != http.StatusNoContent {
		t.Errorf("next /64 = %d, want through on the /48's second token", code)
	}
	rec = post("2001:db8:1:3::1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Errorf("third /64 = %d Retry-After %q, want 429 and 2 (the /48 at 0.5/s)", rec.Code, rec.Header().Get("Retry-After"))
	}
}

// A misconfigured stack fails the boot; one Limiter twice would lock its mutex twice.
func TestNewTiers_whenMisconfigured_thenPanics(t *testing.T) {
	l := New(Options{Rate: 1, Burst: 1})
	for name, tiers := range map[string][]Tier{
		"no tiers":         nil,
		"no limiter":       {{Key: ClientKey(1)}},
		"no key":           {{Limiter: l}},
		"limiter repeated": {{Limiter: l, Key: ClientKey(1)}, {Limiter: l, Key: ClientPrefixKey(1, 32, 48)}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("no panic for %d tiers", len(tiers))
				}
			}()
			NewTiers(tiers...)
		})
	}
}

// Check and charge are one critical section over every tier: parallel requests admit exactly the tightest burst,
// and two Tiers over the same Limiters in opposite order cannot deadlock (a lock-order bug hangs here).
func TestTiers_whenConcurrent_thenTightestBurstAndNoDeadlock(t *testing.T) {
	narrow, wide := New(Options{Rate: 1e-9, Burst: 100}), New(Options{Rate: 1e-9, Burst: 150})
	ab := NewTiers(Tier{Limiter: narrow, Key: ClientKey(1)}, Tier{Limiter: wide, Key: ClientPrefixKey(1, 32, 48)})
	ba := NewTiers(Tier{Limiter: wide, Key: ClientPrefixKey(1, 32, 48)}, Tier{Limiter: narrow, Key: ClientKey(1)})
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := range 400 {
		tiers := ab
		if i%2 == 1 {
			tiers = ba
		}
		wg.Go(func() {
			if ok, _ := tiers.Take(from("2001:db8:1:1::1"), 1); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 100 {
		t.Errorf("allowed %d of 400 concurrent Takes, want the /64's burst of 100", allowed)
	}
	if got := wide.RetryAfter("2001:db8:1::/48"); got != 0 {
		t.Errorf("wide tier blocked (%s): refused requests were charged", got)
	}
}

func ExamplePrefixKey() {
	fmt.Println(PrefixKey(24, 48)("2001:db8:1:2:3:4:5:6"))
	fmt.Println(PrefixKey(24, 48)("203.0.113.7:51234"))
	// Output:
	// 2001:db8:1::/48
	// 203.0.113.0/24
}
