package sse

import "sync"

// HubOptions shapes NewHub. The zero value is a 32-deep buffer per subscriber and no caps.
type HubOptions struct {
	// Buffer is each subscriber's channel depth; 0 means 32. A subscriber that falls this far behind loses values
	// (Publish) or its oldest ones (PublishLatest): one slow reader never stalls the publisher or the others.
	Buffer int
	// Max caps the open subscriptions; 0 is no cap. A stream held for hours is outside any request rate limit, so
	// on a public route this is the only bound there is.
	Max int
	// MaxPerKey caps the subscriptions per key (a client identity, as the rate limiter keys it), so one caller
	// cannot take every slot; 0 is no cap. The empty key is counted only against Max.
	MaxPerKey int
}

// Hub fans each published value out to every subscriber. Safe for concurrent use.
type Hub[T any] struct {
	o      HubOptions
	mu     sync.Mutex // also orders every send against every close: none sends on a closed channel
	subs   map[*subscriber[T]]struct{}
	perKey map[string]int
	closed bool
}

type subscriber[T any] struct {
	ch  chan T
	key string
}

// NewHub returns a hub shaped by o.
func NewHub[T any](o HubOptions) *Hub[T] {
	if o.Buffer <= 0 {
		o.Buffer = 32
	}
	return &Hub[T]{o: o, subs: map[*subscriber[T]]struct{}{}, perKey: map[string]int{}}
}

// Subscribe opens a subscription for key ("" for none): the channel the values arrive on, and cancel, which ends
// it and closes the channel (safe to call twice, and after Close). ok is false when a cap is reached or the hub is
// closed: answer 503 with a Retry-After (httpx.SetRetryAfter) rather than open a stream that is never fed.
func (h *Hub[T]) Subscribe(key string) (ch <-chan T, cancel func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || (h.o.Max > 0 && len(h.subs) >= h.o.Max) ||
		(key != "" && h.o.MaxPerKey > 0 && h.perKey[key] >= h.o.MaxPerKey) {
		return nil, func() {}, false
	}
	s := &subscriber[T]{ch: make(chan T, h.o.Buffer), key: key}
	h.subs[s] = struct{}{}
	if key != "" {
		h.perKey[key]++
	}
	var once sync.Once
	return s.ch, func() { once.Do(func() { h.remove(s) }) }, true
}

func (h *Hub[T]) remove(s *subscriber[T]) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return // Close got there first
	}
	delete(h.subs, s)
	close(s.ch)
	if s.key != "" {
		if h.perKey[s.key]--; h.perKey[s.key] <= 0 {
			delete(h.perKey, s.key)
		}
	}
}

// Publish sends v to every subscriber, dropping it for one whose buffer is full: for events where each one counts
// and a reader that far behind reloads anyway.
func (h *Hub[T]) Publish(v T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- v:
		default:
		}
	}
}

// PublishLatest sends v to every subscriber, evicting a full one's oldest queued value to make room: for state,
// where the newest value supersedes what is queued and dropping it would leave the reader on a stale one.
func (h *Hub[T]) PublishLatest(v T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.ch <- v:
			continue
		default:
		}
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- v:
		default:
		}
	}
}

// Len is the number of open subscriptions.
func (h *Hub[T]) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close ends every subscription, closing its channel, so every stream's loop returns; later Subscribe calls are
// refused and later publishes do nothing. Safe to call more than once.
func (h *Hub[T]) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		close(s.ch)
		delete(h.subs, s)
	}
	clear(h.perKey)
}
