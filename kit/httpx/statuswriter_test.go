package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// 103 Early Hints goes through and is not the response's status.
	sw = NewStatusWriter(httptest.NewRecorder())
	sw.WriteHeader(http.StatusEarlyHints)
	if sw.Status() != 0 {
		t.Errorf("status after a 1xx = %d, want 0", sw.Status())
	}
	sw.WriteHeader(http.StatusOK)
	if sw.Status() != http.StatusOK {
		t.Errorf("status after 103 then 200 = %d, want 200", sw.Status())
	}
}

// countingWriter is a service's own wrapper: it embeds StatusWriter and overrides Write.
type countingWriter struct {
	*StatusWriter
	n int
}

func (w *countingWriter) Write(b []byte) (int, error) {
	w.n += len(b)
	return w.StatusWriter.Write(b)
}

// StatusWriter has no ReadFrom of its own, so io.Copy into an embedding wrapper still goes through its Write.
func TestStatusWriterEmbeddingSeesEveryByte(t *testing.T) {
	cw := &countingWriter{StatusWriter: NewStatusWriter(&readerFromRecorder{ResponseRecorder: httptest.NewRecorder()})}
	if _, err := io.Copy(cw, struct{ io.Reader }{strings.NewReader("twelve bytes")}); err != nil {
		t.Fatal(err)
	}
	if cw.n != 12 {
		t.Errorf("the wrapper's Write counted %d of 12 bytes", cw.n)
	}
}
