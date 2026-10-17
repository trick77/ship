package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// MaxBody bounds every request body at limit bytes, so a handler that reads r.Body on its own cannot be fed an
// unbounded one. DecodeJSON's per-route limit composes with it: the smaller one wins. A read past the limit fails
// with *http.MaxBytesError and the connection closes after the response. A request without a body keeps
// http.NoBody, which code further in may compare against. A limit below one panics.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	if limit < 1 {
		panic("httpx.MaxBody: limit below one byte")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(base(w), r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DecodeOptions shapes DecodeJSONWith. The zero value is DecodeJSON.
type DecodeOptions struct {
	// BadRequest is the 400's message for a body that does not parse; "" means "malformed request". It reaches the
	// client: a fixed phrase naming what the route wants ("url is required"), never the parser's error.
	BadRequest string
	// AllowEmpty accepts an empty body and leaves dst as it was: a route where a bare POST means the default. A body
	// that is there must still parse.
	AllowEmpty bool
}

// DecodeJSON reads at most limit bytes of JSON from r into dst. It answers 413 for a body over the limit and 400 for
// one that does not parse, an empty one included, and returns false for both, so the caller just returns. Checks on
// the fields stay with the caller. A limit below one panics.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	return DecodeJSONWith(w, r, dst, limit, DecodeOptions{})
}

// DecodeJSONWith is DecodeJSON with o.
func DecodeJSONWith(w http.ResponseWriter, r *http.Request, dst any, limit int64, o DecodeOptions) bool {
	if limit < 1 {
		panic("httpx.DecodeJSON: limit below one byte")
	}
	r.Body = http.MaxBytesReader(base(w), r.Body, limit)
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(dst)
	if err == nil {
		// One value and nothing after it: a second one, or junk, is a body that does not parse, and the rest of an
		// oversized body is read until the limit says so.
		if err = dec.Decode(&struct{}{}); errors.Is(err, io.EOF) {
			return true
		} else if err == nil {
			err = errors.New("data after the JSON value")
		}
	}
	var tooLarge *http.MaxBytesError
	switch {
	case o.AllowEmpty && errors.Is(err, io.EOF):
		return true
	case errors.As(err, &tooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	msg := o.BadRequest
	if msg == "" {
		msg = "malformed request"
	}
	WriteError(w, http.StatusBadRequest, msg)
	return false
}

// base is the innermost writer behind the Unwrap chain (StatusWriter, headerHook). http.MaxBytesReader looks there
// for net/http's own response, which it tells to close the connection after the 413: behind a wrapper the client
// keeps sending the rest of a huge body on a connection the server no longer reads.
func base(w http.ResponseWriter) http.ResponseWriter {
	for {
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return w
		}
		w = u.Unwrap()
	}
}
