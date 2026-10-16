package httpx

import (
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
		// The caller still picks the status: a header already written would keep the recorder's 200.
		rec.WriteHeader(503)
		if rec.Body.Len() != 0 || rec.Code != 503 {
			t.Errorf("%s: wrote a response (%d, %q), want the header only", d, rec.Code, rec.Body.String())
		}
	}
}
