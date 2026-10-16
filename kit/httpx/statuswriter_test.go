package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := NewStatusWriter(rec)
	if sw.Status() != 0 {
		t.Errorf("status before any write = %d", sw.Status())
	}
	if sw.Unwrap() != rec {
		t.Error("Unwrap is not the wrapped writer")
	}
	if _, err := sw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if sw.Status() != http.StatusOK {
		t.Errorf("status after a body without a header = %d, want 200", sw.Status())
	}

	rec = httptest.NewRecorder()
	sw = NewStatusWriter(rec)
	sw.WriteHeader(http.StatusNotFound)
	sw.WriteHeader(http.StatusOK) // superfluous, as net/http treats it: the first one stands
	if sw.Status() != http.StatusNotFound || rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, recorded %d; want 404", sw.Status(), rec.Code)
	}
}
