package httpx

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSetRetryAfter(t *testing.T) {
	for d, want := range map[time.Duration]string{
		179500 * time.Millisecond: "180",
		time.Millisecond:          "1",
		2 * time.Second:           "2",
		0:                         "1",
		-time.Minute:              "1",
	} {
		rec := httptest.NewRecorder()
		SetRetryAfter(rec, d)
		if got := rec.Header().Get("Retry-After"); got != want {
			t.Errorf("%s: Retry-After %q, want %q", d, got, want)
		}
		if rec.Body.Len() != 0 || rec.Code != 200 {
			t.Errorf("%s: wrote a response (%d, %q), want the header only", d, rec.Code, rec.Body.String())
		}
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
	if err.Error() != ErrThrottled.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), ErrThrottled.Error())
	}
	now = now.Add(200 * time.Millisecond)
	if te.RetryAfter != 500*time.Millisecond || th.RetryAfter("a") != 300*time.Millisecond {
		t.Errorf("after 200ms: refusal's %s (want 500ms kept), RetryAfter %s (want 300ms)", te.RetryAfter, th.RetryAfter("a"))
	}
	if errors.Is(err, errors.New("too many attempts")) {
		t.Error("errors.Is matched a foreign error by its text")
	}
}
