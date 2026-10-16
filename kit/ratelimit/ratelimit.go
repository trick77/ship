// Package ratelimit is the per-caller token bucket a public endpoint is throttled with: a burst a caller may spend at
// once, refilled at a steady rate, in a map bounded however many callers arrive. Keyed by Key: an IPv6 caller by its
// /64, since rotating the low 64 bits is free and a per-address bucket over v6 bounds nobody. Throttle, beside it,
// spaces one key's attempts apart (a login form): the gate on tries, where Limiter meters volume.
//
//	lim := ratelimit.New(ratelimit.Options{Rate: 50.0 / 60, Burst: 50})
//	api = lim.Middleware(ratelimit.ClientKey(1))(api)
//
// Take splits the gate from the cost, for a limit that meters an outcome rather than a request (the 404s a scanner
// causes): Take(key, cost, penalty) refuses AND charges penalty when the bucket holds no whole token, and spends cost
// when it does, both under one lock. Reserve the outcome's cost up front and refund it after when the outcome turned
// out free, or parallel requests all pass the gate before any of them is charged:
//
//	ok, wait := lim.Take(key, 1, 1) // refused or 404: one token
//	if !ok {
//		httpx.TooManyRequests(w, wait)
//		return
//	}
//	if status != http.StatusNotFound {
//		lim.Charge(key, -1) // free outcome, the reservation goes back
//	}
//
// The charge on refusal is what runs a bucket into debt, down to -Burst, so a caller that keeps knocking while blocked
// stays blocked for longer than one refill; charging only outcomes never goes below 0. A refund never lifts a bucket
// past Burst, so one that refilled while the request ran loses that much of it, and a refund to a key pruned
// meanwhile is dropped. The reservation is the price of the atomic gate: a caller's requests in flight hold tokens
// until they finish, so one caller's concurrency beyond its balance is refused (and penalised) whatever the outcomes;
// size Burst above what a legitimate client runs at once. Permitted and Charge are the parts of Take, for a gate
// that may race.
package ratelimit

import (
	"cmp"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trick77/ship/kit/httpx"
)

// DefaultMaxKeys bounds the buckets a Limiter holds when Options.MaxKeys is 0.
const DefaultMaxKeys = 10_000

// pruneEvery is how often a new key may trigger a pass over the map for full buckets; a full map always does.
const pruneEvery = time.Minute

// Options configures a Limiter.
type Options struct {
	// Rate is the tokens refilled per second (50 a minute: 50.0/60), above 0.
	Rate float64
	// Burst is what a bucket holds, the requests a caller may make at once, at least 1.
	Burst float64
	// MaxKeys bounds the buckets held, DefaultMaxKeys when 0. The map is keyed by caller: without a bound, an
	// allocation the caller controls. At the bound, full buckets go first, then the longest-idle tenth, blocked
	// buckets last.
	MaxKeys int
	// Now is the clock, time.Now when nil.
	Now func() time.Time
}

// Limiter is a set of token buckets, one per key. Safe for concurrent use.
type Limiter struct {
	o         Options
	mu        sync.Mutex
	buckets   map[string]*bucket
	lastPrune time.Time
}

type bucket struct {
	tokens float64
	last   time.Time // when tokens was last brought up to date
}

// New returns a Limiter. A Rate that is not above 0 or a Burst below 1 (zero values included, NaN and infinities
// too) panics: a misconfigured limit fails the boot, never refuses or admits everyone at run time.
func New(o Options) *Limiter {
	if !(o.Rate > 0) || math.IsInf(o.Rate, 0) {
		panic(fmt.Sprintf("ratelimit.New: Rate %v, want a finite rate above 0", o.Rate))
	}
	if !(o.Burst >= 1) || math.IsInf(o.Burst, 0) {
		panic(fmt.Sprintf("ratelimit.New: Burst %v, want a finite burst of at least 1", o.Burst))
	}
	if o.MaxKeys <= 0 {
		o.MaxKeys = DefaultMaxKeys
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Limiter{o: o, buckets: map[string]*bucket{}}
}

// Allow spends one token of key's bucket and reports whether there was one: Take(key, 1, 0).
func (l *Limiter) Allow(key string) bool {
	ok, _ := l.Take(key, 1, 0)
	return ok
}

// Take is Permitted and Charge under one lock: when key's bucket holds a whole token it spends cost and reports true,
// otherwise it spends penalty and reports false with the wait, after the penalty, until the next token. The gate is
// Permitted's, one whole token whatever cost is: a cost above the balance is admitted and runs the bucket into debt,
// as Charge does, and a negative cost or penalty refunds. Permitted then Charge is two locks: parallel requests for
// one key can all pass Permitted before any of them is charged.
func (l *Limiter) Take(key string, cost, penalty float64) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refilled(key)
	if b.tokens < 1 {
		l.spend(b, penalty)
		return false, l.wait(b.tokens)
	}
	l.spend(b, cost)
	return true, 0
}

// Permitted reports whether key's bucket holds a whole token, without spending it. A gate in front of a later
// Charge races its parallel requests through; Take is the atomic form.
func (l *Limiter) Permitted(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refilled(key).tokens >= 1
}

// Charge spends cost tokens of key's bucket, into debt if need be, down to -Burst: the deepest a block goes is one
// burst's worth of refill on top of the wait for a token. A negative cost refunds, never past Burst; a refund to a
// key without a bucket is dropped, since the fresh one it would create is full anyway, and creating it could prune
// other callers' records.
func (l *Limiter) Charge(key string, cost float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.buckets[key]; !ok && !(cost > 0) {
		return
	}
	l.spend(l.refilled(key), cost)
}

// spend takes cost off b, down to -Burst and, for a refund, up to Burst. A NaN cost charges nothing: stored, it would
// leave a bucket no comparison refuses, admitting its key for good. Called with mu held.
func (l *Limiter) spend(b *bucket, cost float64) {
	if math.IsNaN(cost) {
		return
	}
	b.tokens = min(max(b.tokens-cost, -l.o.Burst), l.o.Burst)
}

// RetryAfter is how long until key's bucket holds a whole token, 0 when it does now.
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return 0
	}
	return l.wait(l.tokensAt(b, l.o.Now()))
}

// Len is the number of buckets held.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Middleware answers a request whose key (the caller, from key) has no token left with httpx.TooManyRequests and
// that caller's own Retry-After, the wait until its next token.
func (l *Limiter) Middleware(key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, wait := l.Take(key(r), 1, 0); !ok {
				httpx.TooManyRequests(w, wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// wait is how long a balance of tokens takes to reach one.
func (l *Limiter) wait(tokens float64) time.Duration {
	if tokens >= 1 {
		return 0
	}
	return time.Duration((1 - tokens) / l.o.Rate * float64(time.Second))
}

// tokensAt is b's balance at now, capped at Burst.
func (l *Limiter) tokensAt(b *bucket, now time.Time) float64 {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		return math.Min(b.tokens+elapsed*l.o.Rate, l.o.Burst)
	}
	return b.tokens
}

// refilled returns key's bucket brought up to now, creating a full one for a new key. Called with mu held.
func (l *Limiter) refilled(key string) *bucket {
	now := l.o.Now()
	b, ok := l.buckets[key]
	if !ok {
		l.prune(now)
		b = &bucket{tokens: l.o.Burst, last: now}
		l.buckets[key] = b
		return b
	}
	b.tokens, b.last = l.tokensAt(b, now), now
	return b
}

// prune makes room before a new key: at most once a pruneEvery, or whenever the map is full, it drops every full
// bucket. A full bucket is indistinguishable from the fresh one a returning caller would get, so dropping it forgives
// nothing; a bucket still in debt or part-spent stays, so pausing never clears a caller's record.
//
// A map still full after that loses a tenth, the bound winning over the record: longest idle first, blocked buckets
// (no whole token, debt included) last. Last, because the bound is reachable by anyone with fresh keys (a /48 is
// 65,536 /64s): evicting by age alone would let a blocked scanner wipe its own record by spraying new keys, each of
// which still holds tokens after its one request. Called with mu held.
func (l *Limiter) prune(now time.Time) {
	full := len(l.buckets) >= l.o.MaxKeys
	if !full && now.Sub(l.lastPrune) < pruneEvery {
		return
	}
	l.lastPrune = now
	for k, b := range l.buckets {
		if l.tokensAt(b, now) >= l.o.Burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < l.o.MaxKeys {
		return
	}
	type entry struct {
		key     string
		blocked bool
		last    time.Time
	}
	entries := make([]entry, 0, len(l.buckets))
	for k, b := range l.buckets {
		entries = append(entries, entry{k, l.tokensAt(b, now) < 1, b.last})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.blocked != b.blocked {
			if a.blocked {
				return 1
			}
			return -1
		}
		return cmp.Compare(a.last.UnixNano(), b.last.UnixNano())
	})
	for _, e := range entries[:max(len(entries)/10, 1)] {
		delete(l.buckets, e.key)
	}
}

// Key is the bucket key for a client address: an IPv4 address as is, an IPv6 address as its /64
// ("2001:db8:1:2::/64"). A /64 is the smallest block one subscriber is routinely given and the smallest not free to
// rotate within; IPv4 is scarce enough that one address is a real identity, and masking it would merge a whole
// office behind one neighbour. A port or brackets a proxy left on ("203.0.113.7:51234", "[2001:db8::1]:443") are
// dropped, or every source port would be a fresh bucket. A value that is no address is returned as is: keying on
// the literal still bounds whoever keeps sending it.
func Key(ip string) string {
	a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]"))
	if err != nil {
		ap, perr := netip.ParseAddrPort(ip)
		if perr != nil {
			return ip
		}
		a = ap.Addr()
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, _ := a.WithZone("").Prefix(64)
	return p.String()
}

// ClientKey keys a request by its caller: httpx.ClientIP with trustedHops, through Key.
func ClientKey(trustedHops int) func(*http.Request) string {
	return func(r *http.Request) string { return Key(httpx.ClientIP(r, trustedHops)) }
}
