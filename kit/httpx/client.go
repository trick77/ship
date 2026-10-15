package httpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ClientIP is the request's client address. X-Forwarded-For is a list a caller can prepend anything to while each
// proxy appends the peer it saw, so the trustworthy entry is trustedHops from the right, never the leftmost. With
// trustedHops 0, or no usable header, it is the TCP peer. Use the number of proxies in front of the app: 1 behind
// a single Traefik.
func ClientIP(r *http.Request, trustedHops int) string {
	if trustedHops > 0 {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			parts := strings.Split(xff, ",")
			if i := len(parts) - trustedHops; i >= 0 {
				if ip := strings.TrimSpace(parts[i]); ip != "" {
					return ip
				}
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ErrThrottled is Throttle.Wait's answer to a key with more queued than maxWait covers. Throttle.RetryAfter is how
// long until that key would be let into the queue again, the 429's Retry-After.
var ErrThrottled = errors.New("too many attempts")

// minPrune is the map size below which Throttle never sweeps.
const minPrune = 1024

// Throttle spaces one key's attempts gap apart: a login form keyed by ClientIP caps guessing at 1/gap per client
// however many connections it opens, while a flood from one client cannot lock anyone else out (a shared limit would
// hand every visitor that lever).
type Throttle struct {
	gap, maxWait time.Duration
	now          func() time.Time
	mu           sync.Mutex
	next         map[string]time.Time
	pruneAt      int // map size that triggers the next sweep
}

// NewThrottle spaces attempts gap apart and refuses a key once it would queue past maxWait.
func NewThrottle(gap, maxWait time.Duration) *Throttle {
	return &Throttle{gap: gap, maxWait: maxWait, now: time.Now, next: map[string]time.Time{}, pruneAt: minPrune}
}

// turn books key's next slot: the slot's start, how long to wait for it, and false when the queue is too long.
func (t *Throttle) turn(key string) (time.Time, time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// Past entries carry no information; drop them before the map grows with every address that ever tried. A sweep
	// runs once the map has doubled since the last one left it: amortized O(1) a call while every entry is live, and
	// the map never holds more than twice the keys queued within the last maxWait+gap (or minPrune).
	if len(t.next) > t.pruneAt {
		for k, at := range t.next {
			if at.Before(now) {
				delete(t.next, k)
			}
		}
		t.pruneAt = max(minPrune, 2*len(t.next))
	}
	at := t.next[key]
	if at.Before(now) {
		at = now
	}
	if at.Sub(now) > t.maxWait {
		return time.Time{}, 0, false
	}
	t.next[key] = at.Add(t.gap)
	return at, at.Sub(now), true
}

// release gives back the slot at that turn booked for an attempt that gave up before it, so key's next attempt is
// not pushed back by one that never ran. Only the latest booking can be given back: with a later one queued behind
// it, moving next would hand that later slot out twice, so the gap stays booked.
func (t *Throttle) release(key string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if next, ok := t.next[key]; ok && next.Equal(at.Add(t.gap)) {
		t.next[key] = at
	}
}

// Wait blocks until key's turn: ErrThrottled when the queue is too long, ctx's error when the caller gave up. A
// caller that gives up frees its turn when no later attempt of key has queued behind it.
func (t *Throttle) Wait(ctx context.Context, key string) error {
	at, d, ok := t.turn(key)
	if !ok {
		return ErrThrottled
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		t.release(key, at)
		return ctx.Err()
	}
}

// RetryAfter is how long until key's next Wait would be queued rather than refused with ErrThrottled, 0 when it would
// be now; the queued Wait may still block up to maxWait. It books nothing and reads the queue under its own lock, so
// after a refusal it is that key's wait as of the call, not of the refusal: other attempts may have queued since.
func (t *Throttle) RetryAfter(key string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	next, ok := t.next[key]
	if !ok {
		return 0
	}
	return max(next.Sub(t.now())-t.maxWait, 0)
}
