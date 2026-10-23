package httpx

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// readerFromRecorder is net/http's response in the one way that matters here: it has ReadFrom, the path to sendfile.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFrom int
}

func (w *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	w.readFrom++
	return io.Copy(w.ResponseRecorder, src)
}

// noReaderFrom is a writer with no ReadFrom: the copy fallback.
type noReaderFrom struct{ http.ResponseWriter }

func TestStatusWriterReadFrom(t *testing.T) {
	base := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	base.Header().Set("Content-Type", "video/mp4")
	sw := statusReaderFrom{NewStatusWriter(base)}
	n, err := sw.ReadFrom(strings.NewReader("body"))
	if err != nil || n != 4 {
		t.Fatalf("ReadFrom = %d, %v", n, err)
	}
	if base.readFrom != 1 {
		t.Errorf("base ReadFrom called %d times, want 1 (sendfile lost)", base.readFrom)
	}
	if sw.Status() != http.StatusOK || base.Body.String() != "body" {
		t.Errorf("status %d, body %q", sw.Status(), base.Body.String())
	}

	// No type: the sniffed head goes through Write, the rest still reaches the base's ReadFrom.
	base = &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw = statusReaderFrom{NewStatusWriter(base)}
	long := strings.Repeat("b", 4096)
	if n, err := sw.ReadFrom(strings.NewReader(long)); err != nil || n != 4096 || base.Body.String() != long {
		t.Fatalf("untyped ReadFrom = %d, %v, body %d bytes", n, err, base.Body.Len())
	}
	if base.readFrom != 1 {
		t.Errorf("untyped: base ReadFrom called %d times, want 1 (sendfile lost)", base.readFrom)
	}

	// A header written first stands.
	base = &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw = statusReaderFrom{NewStatusWriter(base)}
	sw.WriteHeader(http.StatusPartialContent)
	if _, err := sw.ReadFrom(strings.NewReader("x")); err != nil || sw.Status() != http.StatusPartialContent {
		t.Errorf("status %d, err %v; want 206", sw.Status(), err)
	}

	// A base without ReadFrom still gets the bytes.
	rec := httptest.NewRecorder()
	sw = statusReaderFrom{NewStatusWriter(noReaderFrom{rec})}
	if n, err := sw.ReadFrom(strings.NewReader("copied")); err != nil || n != 6 || rec.Body.String() != "copied" {
		t.Errorf("fallback = %d, %v, body %q", n, err, rec.Body.String())
	}
}

// http.ServeContent behind the whole chain reaches the server's ReadFrom (sendfile for an *os.File).
func TestChainKeepsReadFrom(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, htmlOnly := range []bool{false, true} {
		h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "v.mp4", time.Time{}, strings.NewReader(strings.Repeat("v", 4096)))
		}),
			AccessLog(log, AccessLogOptions{}),
			SecurityHeadersWith(SecurityOptions{HTMLOnly: htmlOnly}),
			RecoverWith(log, RecoverOptions{}),
		)
		base := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/v", nil))
		if base.readFrom != 1 || base.Body.Len() != 4096 {
			t.Errorf("HTMLOnly=%v: base ReadFrom called %d times, body %d bytes", htmlOnly, base.readFrom, base.Body.Len())
		}
	}
}

// The hook still sees the first bytes when ReadFrom sends the header: an unset type is sniffed from them.
func TestHeaderHookReadFromSniffs(t *testing.T) {
	h := SecurityHeadersWith(SecurityOptions{HTMLOnly: true})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("<!doctype html><p>page"))
	}))
	base := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/", nil))
	if base.Header().Get("Content-Security-Policy") == "" {
		t.Error("an HTML body sent by ReadFrom got no CSP")
	}
	if base.Body.String() != "<!doctype html><p>page" {
		t.Errorf("body = %q", base.Body.String())
	}
}

// A ReadFrom before any header records nothing until bytes go out: a src that fails first leaves the status to the
// handler's own error (and Recover's 500), never a 200 that was not sent.
func TestStatusWriterReadFromFailingSource(t *testing.T) {
	sw := statusReaderFrom{NewStatusWriter(&readerFromRecorder{ResponseRecorder: httptest.NewRecorder()})}
	if _, err := sw.ReadFrom(failingReader{}); err == nil {
		t.Fatal("ReadFrom of a failing src = nil error")
	}
	if sw.Status() != 0 {
		t.Errorf("status after a failed first read = %d, want 0", sw.Status())
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("broken file") }

// slowReader hands out one short chunk per read, as a quiet stream does.
type slowReader struct{ chunks []string }

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

// With a Content-Type set nothing is sniffed: the hook decides on the type before any byte, and the stream goes
// straight to the base's ReadFrom, a short first chunk not held back for 512 bytes. Without one, the hook waits for
// what net/http sniffs from.
func TestHeaderHookReadFromFirstChunk(t *testing.T) {
	for _, tt := range []struct {
		ct     string
		chunks []string
		want   []byte
	}{
		{"text/event-stream", []string{"tick\n", "tock\n"}, nil},
		{"", []string{"<", "!doctype html>", "<p>page"}, []byte("<!doctype html><p>page")},
	} {
		var first []byte
		base := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
		if tt.ct != "" {
			base.Header().Set("Content-Type", tt.ct)
		}
		hook := &headerHook{ResponseWriter: base, hook: func(_ http.Header, body []byte) { first = body }}
		if _, err := hook.ReadFrom(&slowReader{chunks: tt.chunks}); err != nil {
			t.Fatal(err)
		}
		hook.finish()
		if string(first) != string(tt.want) || (first == nil) != (tt.want == nil) {
			t.Errorf("type %q: hook saw %q, want %q", tt.ct, first, tt.want)
		}
		if tt.ct != "" && base.readFrom != 1 {
			t.Errorf("type %q: base ReadFrom called %d times, want 1", tt.ct, base.readFrom)
		}
		if got := base.Body.String(); got != strings.Join(tt.chunks, "") {
			t.Errorf("type %q: body %q", tt.ct, got)
		}
	}
}

// emptyReader returns (0, nil) for ever.
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

func TestReadFromGivesUpOnNoProgress(t *testing.T) {
	sw := statusReaderFrom{NewStatusWriter(&readerFromRecorder{ResponseRecorder: httptest.NewRecorder()})}
	if _, err := sw.ReadFrom(emptyReader{}); !errors.Is(err, io.ErrNoProgress) {
		t.Errorf("ReadFrom of a reader that never progresses = %v, want io.ErrNoProgress", err)
	}
}

// Behind a real server: a body over 512 bytes arrives whole, an HTML one gets CSP and a JSON one does not. The source
// hides strings.Reader's WriteTo, so io.Copy takes the ReadFrom path.
func TestHeaderHookReadFromServer(t *testing.T) {
	html := "<!doctype html><p>" + strings.Repeat("x", 2000)
	json := `{"k":"` + strings.Repeat("x", 2000) + `"}`
	srv := httptest.NewServer(SecurityHeadersWith(SecurityOptions{HTMLOnly: true})(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			body := html
			if r.URL.Path == "/json" {
				w.Header().Set("Content-Type", "application/json")
				body = json
			}
			if _, ok := w.(io.ReaderFrom); !ok {
				t.Error("the hook's writer has no ReadFrom")
			}
			_, _ = io.Copy(w, struct{ io.Reader }{strings.NewReader(body)})
		})))
	defer srv.Close()
	for path, want := range map[string]string{"/": html, "/json": json} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if string(b) != want {
			t.Errorf("%s: body %d bytes, want %d", path, len(b), len(want))
		}
		if csp := res.Header.Get("Content-Security-Policy"); (csp != "") != (path == "/") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
}

// Behind Recover's StatusWriter the hook still decides on what net/http sniffs: a short first read of an HTML body
// with no Content-Type is not typed text/plain (and left without CSP) from its first byte.
func TestChainReadFromSniffsShortFirstRead(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, &slowReader{chunks: []string{"<", "!doctype html>", "<p>page"}})
	}),
		AccessLog(log, AccessLogOptions{}),
		SecurityHeadersWith(SecurityOptions{HTMLOnly: true}),
		RecoverWith(log, RecoverOptions{}),
	)
	base := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/", nil))
	if base.Header().Get("Content-Security-Policy") == "" {
		t.Error("an HTML body whose first read is short got no CSP")
	}
	if base.Body.String() != "<!doctype html><p>page" {
		t.Errorf("body = %q", base.Body.String())
	}
}

// A writer with no ReadFrom under the hook (HTTP/2's) sniffs the same way: the copy fallback reads what net/http
// sniffs before the first Write.
func TestHeaderHookReadFromNoReaderFromSniffs(t *testing.T) {
	var first string
	hook := &headerHook{ResponseWriter: noReaderFrom{httptest.NewRecorder()}, hook: func(_ http.Header, body []byte) {
		first = string(body)
	}}
	if _, err := hook.ReadFrom(&slowReader{chunks: []string{"<", "!doctype html>", "<p>page"}}); err != nil {
		t.Fatal(err)
	}
	hook.finish()
	if first != "<!doctype html><p>page" {
		t.Errorf("hook saw %q, want the whole sniffed head", first)
	}
}

// wrappedEOFReader fails with an error that wraps io.EOF: a truncation, not the clean end io.Copy accepts.
type wrappedEOFReader struct{}

func (wrappedEOFReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), fmt.Errorf("upstream closed: %w", io.EOF)
}

func TestReadFromWrappedEOFIsAnError(t *testing.T) {
	sw := statusReaderFrom{NewStatusWriter(&readerFromRecorder{ResponseRecorder: httptest.NewRecorder()})}
	if _, err := sw.ReadFrom(wrappedEOFReader{}); err == nil {
		t.Error("ReadFrom of a source failing with a wrapped io.EOF = nil error, want the truncation")
	}
}
