package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type body struct {
		URL string `json:"url"`
	}
	tests := []struct {
		name     string
		body     string
		opts     DecodeOptions
		wantOK   bool
		wantCode int
		wantMsg  string
		wantURL  string
	}{
		{name: "valid", body: `{"url":"x"}`, wantOK: true, wantURL: "x"},
		{name: "malformed", body: `{"url":`, wantCode: 400, wantMsg: "malformed request"},
		{name: "empty", body: ``, wantCode: 400, wantMsg: "malformed request"},
		{name: "own message", body: `nope`, opts: DecodeOptions{BadRequest: "url is required"}, wantCode: 400,
			wantMsg: "url is required"},
		{name: "too large", body: `{"url":"` + strings.Repeat("a", 64) + `"}`, wantCode: 413,
			wantMsg: "request body too large"},
		{name: "trailing space", body: `{"url":"x"}` + "\n ", wantOK: true, wantURL: "x"},
		{name: "second value", body: `{"url":"x"} {"url":"y"}`, wantCode: 400, wantMsg: "malformed request"},
		{name: "junk after", body: `{"url":"x"} junk`, wantCode: 400, wantMsg: "malformed request"},
		{name: "over the limit after a value", body: `{"url":"x"}` + strings.Repeat(" ", 64), wantCode: 413,
			wantMsg: "request body too large"},
		{name: "empty allowed", body: ``, opts: DecodeOptions{AllowEmpty: true}, wantOK: true, wantURL: "default"},
		{name: "malformed with empty allowed", body: `{`, opts: DecodeOptions{AllowEmpty: true}, wantCode: 400,
			wantMsg: "malformed request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			dst := body{URL: "default"}
			ok := DecodeJSONWith(rec, r, &dst, 32, tt.opts)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				if dst.URL != tt.wantURL || rec.Body.Len() != 0 {
					t.Errorf("url = %q, body %q; want %q and nothing written", dst.URL, rec.Body, tt.wantURL)
				}
				return
			}
			var got map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if rec.Code != tt.wantCode || got["error"] != tt.wantMsg {
				t.Errorf("answer = %d %q, want %d %q", rec.Code, got["error"], tt.wantCode, tt.wantMsg)
			}
		})
	}
}

// The 413 has to reach net/http's own response through the wrappers, or the connection is not closed after it.
func TestDecodeJSONClosesConnectionBehindWrappers(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		DecodeJSON(w, r, &v, 8)
	}), AccessLog(slog.New(slog.DiscardHandler), AccessLogOptions{}), SecurityHeadersWith(SecurityOptions{HTMLOnly: true}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"k":"`+strings.Repeat("a", 64)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
		t.Errorf("status %d, close %v; want 413 and Connection: close", resp.StatusCode, resp.Close)
	}
}

func TestMaxBody(t *testing.T) {
	var readErr error
	h := MaxBody(8)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123456789")))
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) || mbe.Limit != 8 {
		t.Errorf("read past the limit: err = %v, want *http.MaxBytesError at 8", readErr)
	}

	// No body stays http.NoBody.
	var body io.ReadCloser
	MaxBody(8)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { body = r.Body })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if body != http.NoBody {
		t.Errorf("a GET's body = %T, want http.NoBody", body)
	}

	// The smaller of the two limits wins, whichever side it is on.
	rec := httptest.NewRecorder()
	MaxBody(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		DecodeJSON(w, r, &v, 1<<20)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"k":"0123456789"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("server-wide limit under a looser route limit: status %d, want 413", rec.Code)
	}
}

func TestBodyLimitBelowOnePanics(t *testing.T) {
	for name, f := range map[string]func(){
		"MaxBody":    func() { MaxBody(0) },
		"DecodeJSON": func() { DecodeJSON(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), &struct{}{}, 0) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s with limit 0 did not panic", name)
				}
			}()
			f()
		}()
	}
}
