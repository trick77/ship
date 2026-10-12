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

// ErrThrottled is Throttle.Wait's answer to a key with more queued than maxWait covers.
var ErrThrottled = errors.New("too many attempts")

// Throttle spaces one key's attempts gap apart: a login form keyed by ClientIP caps guessing at 1/gap per client
// however many connections it opens, while a flood from one client cannot lock anyone else out (a shared limit would
// hand every visitor that lever).
type Throttle struct {
	gap, maxWait time.Duration
	now          func() time.Time
	mu           sync.Mutex
	next         map[string]time.Time
}

// NewThrottle spaces attempts gap apart and refuses a key once it would queue past maxWait.
func NewThrottle(gap, maxWait time.Duration) *Throttle {
	return &Throttle{gap: gap, maxWait: maxWait, now: time.Now, next: map[string]time.Time{}}
}

// turn books key's next slot and returns how long to wait for it.
func (t *Throttle) turn(key string) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// Past entries carry no information; drop them before the map grows with every address that ever tried.
	if len(t.next) > 1024 {
		for k, at := range t.next {
			if at.Before(now) {
				delete(t.next, k)
			}
		}
	}
	at := t.next[key]
	if at.Before(now) {
		at = now
	}
	if at.Sub(now) > t.maxWait {
		return 0, false
	}
	t.next[key] = at.Add(t.gap)
	return at.Sub(now), true
}

// Wait blocks until key's turn: ErrThrottled when the queue is too long, ctx's error when the caller gave up.
func (t *Throttle) Wait(ctx context.Context, key string) error {
	d, ok := t.turn(key)
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
		return ctx.Err()
	}
}
