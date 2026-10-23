package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// htmlOnly serves h behind SecurityHeadersWith(HTMLOnly) on a real server and returns the response's header and body.
func htmlOnly(t *testing.T, h http.HandlerFunc, inner ...func(http.Handler) http.Handler) (http.Header, string) {
	t.Helper()
	srv := httptest.NewServer(Chain(h, append([]func(http.Handler) http.Handler{SecurityHeadersWith(SecurityOptions{HTMLOnly: true})}, inner...)...))
	defer srv.Close()
	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.Header, string(b)
}

// noReadFromMiddleware hands next a writer without ReadFrom, as a service's own wrapper may.
func noReadFromMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(noReaderFrom{w}, r) })
}

// An untyped page gets CSP however the handler cuts it: small writes, a copy behind a wrapper without ReadFrom. The
// hook decides on the head net/http sniffs, not on the first write.
func TestHeaderHookDecidesOnTheSniffedHead(t *testing.T) {
	page := "<!doctype html><p>page"
	for name, tt := range map[string]struct {
		h     http.HandlerFunc
		inner []func(http.Handler) http.Handler
	}{
		"small writes": {h: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<")
			_, _ = io.WriteString(w, "!doctype html><p>page")
		}},
		"copy behind a wrapper without ReadFrom": {h: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.Copy(w, &slowReader{chunks: []string{"<", "!doctype html>", "<p>page"}})
		}, inner: []func(http.Handler) http.Handler{noReadFromMiddleware}},
	} {
		hdr, body := htmlOnly(t, tt.h, tt.inner...)
		if body != page || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
			t.Errorf("%s: %q as %q", name, body, hdr.Get("Content-Type"))
		}
		if hdr.Get("Content-Security-Policy") == "" || hdr.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: a page got no CSP or X-Frame-Options", name)
		}
	}
}

// What net/http does not sniff is not held: a nil Content-Type (sniffing off) or an encoded body goes on at the
// first write, and an encoded one, whose type is unknown, is treated as a page.
func TestHeaderHookHoldsOnlyWhatIsSniffed(t *testing.T) {
	for name, set := range map[string]func(http.Header){
		"sniffing off": func(h http.Header) { h["Content-Type"] = nil },
		"encoded":      func(h http.Header) { h.Set("Content-Encoding", "gzip") },
	} {
		var csp string
		base := httptest.NewRecorder()
		hook := &headerHook{ResponseWriter: base, hook: func(_ http.Header, body []byte) {
			if body != nil {
				t.Errorf("%s: hook sniffed %q", name, body)
			}
			csp = "decided"
		}}
		set(hook.Header())
		_, _ = hook.Write([]byte("tick\n"))
		if base.Body.String() != "tick\n" || csp == "" {
			t.Errorf("%s: first write held back (body %q)", name, base.Body.String())
		}
	}
	hdr, _ := htmlOnly(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "identity")
		_, _ = io.WriteString(w, "<!doctype html><p>page")
	})
	if hdr.Get("Content-Security-Policy") == "" {
		t.Error("an encoded body of unknown type got no CSP")
	}
}

// An informational 1xx goes on at once; the decision waits for the final response's type.
func TestHeaderHookDecidesAfterEarlyHints(t *testing.T) {
	base := httptest.NewRecorder()
	var decided string
	hook := &headerHook{ResponseWriter: base, hook: func(h http.Header, _ []byte) { decided = h.Get("Content-Type") }}
	hook.Header().Set("Content-Type", "text/plain")
	hook.WriteHeader(http.StatusEarlyHints)
	if decided != "" {
		t.Fatalf("hook ran on the 103 (%q)", decided)
	}
	hook.Header().Set("Content-Type", "text/html")
	hook.WriteHeader(http.StatusOK)
	if decided != "text/html" {
		t.Errorf("hook decided on %q, want the final text/html", decided)
	}
}

// Flush sends what is held, header first: a stream with no type is not stuck below 512 bytes.
func TestHeaderHookFlushReleases(t *testing.T) {
	base := httptest.NewRecorder()
	ran := false
	hook := &headerHook{ResponseWriter: base, hook: func(http.Header, []byte) { ran = true }}
	_, _ = hook.Write([]byte("tick\n"))
	if ran || base.Body.Len() != 0 {
		t.Fatal("an untyped short write went on before the sniff")
	}
	hook.Flush()
	if !ran || base.Body.String() != "tick\n" || !base.Flushed {
		t.Errorf("after Flush: ran %v, body %q, flushed %v", ran, base.Body.String(), base.Flushed)
	}
}

// A handler that writes nothing leaves the hook alone, as before: net/http sends its empty 200.
func TestHeaderHookUntouchedResponse(t *testing.T) {
	hdr, body := htmlOnly(t, func(http.ResponseWriter, *http.Request) {})
	if body != "" || hdr.Get("Content-Security-Policy") != "" {
		t.Errorf("empty response: body %q, CSP %q", body, hdr.Get("Content-Security-Policy"))
	}
}
