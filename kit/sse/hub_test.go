package sse

import (
	"sync"
	"testing"
)

func recv[T any](t *testing.T, ch <-chan T) (T, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	default:
		var zero T
		return zero, false
	}
}

func TestHubFanOut(t *testing.T) {
	h := NewHub[int](HubOptions{})
	a, cancelA, _ := h.Subscribe("")
	defer cancelA()
	b, cancelB, _ := h.Subscribe("")
	defer cancelB()
	h.Publish(7)
	for i, ch := range []<-chan int{a, b} {
		if v, ok := recv(t, ch); !ok || v != 7 {
			t.Errorf("subscriber %d got %d, %v", i, v, ok)
		}
	}
}

func TestHubCancel(t *testing.T) {
	h := NewHub[int](HubOptions{})
	ch, cancel, _ := h.Subscribe("")
	cancel()
	cancel() // twice is fine
	if _, open := <-ch; open {
		t.Error("channel still open after cancel")
	}
	h.Publish(1) // no send on the closed channel
	if h.Len() != 0 {
		t.Errorf("Len = %d after cancel", h.Len())
	}
}

// A full subscriber drops the value; Publish never blocks on it, and others still get theirs.
func TestHubPublishDropsWhenFull(t *testing.T) {
	h := NewHub[int](HubOptions{Buffer: 2})
	slow, cancelSlow, _ := h.Subscribe("")
	defer cancelSlow()
	fast, cancelFast, _ := h.Subscribe("")
	defer cancelFast()
	for i := range 3 {
		h.Publish(i)
		if v, _ := recv(t, fast); v != i {
			t.Errorf("fast got %d, want %d", v, i)
		}
	}
	if v, _ := recv(t, slow); v != 0 {
		t.Errorf("slow's first = %d, want 0 (the oldest kept)", v)
	}
	if v, _ := recv(t, slow); v != 1 {
		t.Errorf("slow's second = %d, want 1", v)
	}
	if _, ok := recv(t, slow); ok {
		t.Error("slow got the dropped third value")
	}
}

// PublishLatest evicts the oldest queued value instead, so the newest state always lands.
func TestHubPublishLatest(t *testing.T) {
	h := NewHub[int](HubOptions{Buffer: 2})
	ch, cancel, _ := h.Subscribe("")
	defer cancel()
	for i := range 4 {
		h.PublishLatest(i)
	}
	if v, _ := recv(t, ch); v != 2 {
		t.Errorf("first = %d, want 2", v)
	}
	if v, _ := recv(t, ch); v != 3 {
		t.Errorf("second = %d, want 3 (the latest)", v)
	}
}

func TestHubLimits(t *testing.T) {
	h := NewHub[int](HubOptions{Max: 3, MaxPerKey: 2})
	_, c1, ok1 := h.Subscribe("a")
	_, _, ok2 := h.Subscribe("a")
	if _, _, ok := h.Subscribe("a"); !ok1 || !ok2 || ok {
		t.Fatalf("per-key cap: %v %v %v, want true true false", ok1, ok2, ok)
	}
	if _, _, ok := h.Subscribe("b"); !ok {
		t.Fatal("another key refused under the global cap")
	}
	if _, _, ok := h.Subscribe("c"); ok {
		t.Fatal("global cap not enforced")
	}
	c1()
	if _, _, ok := h.Subscribe("a"); !ok {
		t.Error("a cancelled subscription did not free its key's slot")
	}
	// The empty key is no key: only the global cap applies to it.
	h2 := NewHub[int](HubOptions{MaxPerKey: 1})
	for i := range 3 {
		if _, _, ok := h2.Subscribe(""); !ok {
			t.Fatalf("unkeyed subscription %d refused by the per-key cap", i)
		}
	}
}

func TestHubClose(t *testing.T) {
	h := NewHub[int](HubOptions{})
	ch, cancel, _ := h.Subscribe("")
	h.Close()
	h.Close()
	if _, open := <-ch; open {
		t.Error("channel open after Close")
	}
	cancel() // after Close: no double close
	h.Publish(1)
	h.PublishLatest(1)
	if _, _, ok := h.Subscribe(""); ok {
		t.Error("Subscribe after Close succeeded")
	}
}

// Run under -race: publish, cancel and close never send on a closed channel or race on the set.
func TestHubConcurrent(t *testing.T) {
	h := NewHub[int](HubOptions{Buffer: 1})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ch, cancel, ok := h.Subscribe("")
			if !ok {
				return
			}
			for range 50 {
				recv(t, ch)
			}
			cancel()
		})
		wg.Go(func() {
			for j := range 100 {
				if j%2 == 0 {
					h.Publish(i)
				} else {
					h.PublishLatest(i)
				}
			}
		})
	}
	wg.Go(h.Close)
	wg.Wait()
}
