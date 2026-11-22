package ratelimit

import (
	"cmp"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/trick77/ship/kit/httpx"
)

// Tier is one limit of a Tiers: a Limiter and the key a request has in it.
type Tier struct {
	// Limiter holds the tier's buckets, its own Rate and Burst. One Limiter serves one tier of a Tiers.
	Limiter *Limiter
	// Key is the request's bucket key in Limiter: ClientKey per /64, ClientPrefixKey per wider network.
	Key func(*http.Request) string
}

// Tiers is several limits a request must all pass, each a Limiter keyed its own way: a strict one per /64 beside a
// generous one per /48 bounds both one subscriber and a caller rotating through its block's 65,536 /64s. Safe for
// concurrent use; the Limiters stay usable on their own. Only NewTiers makes one: the zero Tiers, which would admit
// everything, panics on use.
type Tiers struct {
	tiers   []Tier  // in lock order: Limiter.seq ascending
	maxCost float64 // the smallest Burst
}

// NewTiers stacks tiers. No tier, a tier without a Limiter or a Key, or one Limiter in two tiers (its lock would be
// taken twice) panics, as New does: a misconfigured limit fails the boot.
func NewTiers(tiers ...Tier) *Tiers {
	if len(tiers) == 0 {
		panic("ratelimit.NewTiers: no tiers")
	}
	for i, t := range tiers {
		if t.Limiter == nil || t.Key == nil {
			panic(fmt.Sprintf("ratelimit.NewTiers: tier %d lacks a Limiter or a Key", i))
		}
	}
	sorted := slices.Clone(tiers)
	slices.SortFunc(sorted, func(a, b Tier) int { return cmp.Compare(a.Limiter.seq, b.Limiter.seq) })
	maxCost := sorted[0].Limiter.o.Burst
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Limiter == sorted[i-1].Limiter {
			panic("ratelimit.NewTiers: one Limiter in two tiers")
		}
		maxCost = min(maxCost, sorted[i].Limiter.o.Burst)
	}
	return &Tiers{tiers: sorted, maxCost: maxCost}
}

// MaxCost is the largest cost Take can admit, the smallest tier's Burst: a batch above it never fits that bucket.
// Size Burst for the largest batch a legitimate client sends, and cap the body at what that many events take.
func (t *Tiers) MaxCost() float64 {
	t.built()
	return t.maxCost
}

// Take admits r at cost (the events in one batch) when every tier's bucket holds cost tokens and spends exactly cost
// on each, or refuses, charges no tier and reports the wait until every refusing tier holds cost: the 429's
// Retry-After, never 0. The gate is exact, unlike Limiter.Take's one whole token: no tier goes into debt, so every
// event admitted is paid for on every tier, and one caller's batch takes from a shared /48 only what it admits.
// A cost below one (an empty batch, a negative or NaN cost) is one: every call is metered, none refunds.
//
// A cost above MaxCost no wait admits. Take refuses it with a wait of 0 (answer 413), charging the one token any call
// costs when every tier holds it; when one does not, the refusal is the 429 for that token. An oversized batch is
// metered like any call, so sending them is not free work for the server.
//
// Check and charge run under every tier's lock at once, taken in one order for all Tiers (the Limiters' creation
// order), so two Tiers sharing Limiters cannot deadlock and parallel requests cannot all pass before any is charged.
// Taking tier by tier and refunding after a later refusal would race instead: the narrow tier stays charged while the
// wide one decides, refusing the caller's parallel requests meanwhile. A refusal creates no bucket either, so a caller
// the /48 refuses cannot fill the /64 map with the /64s it rotates through. Keys are computed before the locks.
func (t *Tiers) Take(r *http.Request, cost float64) (ok bool, retryAfter time.Duration) {
	t.built()
	if !(cost >= 1) {
		cost = 1
	}
	oversized := cost > t.maxCost
	if oversized {
		cost = 1
	}
	keys := make([]string, len(t.tiers))
	for i, tier := range t.tiers {
		keys[i] = tier.Key(r)
	}
	for _, tier := range t.tiers {
		tier.Limiter.mu.Lock()
	}
	defer func() {
		for i := len(t.tiers) - 1; i >= 0; i-- {
			t.tiers[i].Limiter.mu.Unlock()
		}
	}()
	refused := false
	for i, tier := range t.tiers {
		l := tier.Limiter
		b, held := l.buckets[keys[i]]
		if !held {
			continue // a fresh bucket holds Burst, at least MaxCost
		}
		if tokens := l.tokensAt(b, l.o.Now()); tokens < cost {
			refused, retryAfter = true, max(retryAfter, l.waitFor(tokens, cost))
		}
	}
	if refused {
		return false, retryAfter
	}
	for i, tier := range t.tiers {
		tier.Limiter.spend(tier.Limiter.refilled(keys[i]), cost)
	}
	return !oversized, 0
}

// Middleware answers a request some tier refuses with httpx.TooManyRequests and Take's wait, at a cost of one per
// request. A handler that meters what the body holds (the events in a batch) calls Take itself once it has parsed it.
// The zero Tiers panics here, at construction.
func (t *Tiers) Middleware(next http.Handler) http.Handler {
	t.built()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := t.Take(r, 1); !ok {
			httpx.TooManyRequests(w, wait)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// built panics on a Tiers NewTiers did not make: with no tiers, every request would pass.
func (t *Tiers) built() {
	if len(t.tiers) == 0 {
		panic("ratelimit: Tiers used without NewTiers")
	}
}

// waitFor is how long a balance of tokens takes to reach need: at least a nanosecond, since a refusal's wait of 0 is
// Tiers.Take's oversized answer, and at most the longest Duration.
func (l *Limiter) waitFor(tokens, need float64) time.Duration {
	d := (need - tokens) / l.o.Rate * float64(time.Second)
	if d >= math.MaxInt64 {
		return math.MaxInt64
	}
	return max(time.Duration(d), time.Nanosecond)
}
