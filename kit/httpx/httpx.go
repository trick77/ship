// Package httpx is the HTTP plumbing every service repeats: JSON responses, health probes, the server lifecycle, the
// container healthcheck and the middleware chain (access log with request id, panic recovery, cross-origin
// protection, security headers). net/http only.
package httpx

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"
)

// WriteJSON writes v as JSON with status. Never cached: API bodies describe state that a cached copy would misreport,
// and some carry freshly minted secrets.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the canonical error body, {"error": msg}. msg reaches the client: an internal detail belongs in
// the log, the client gets a fixed phrase.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// TooManyRequests writes a 429 with Retry-After: retryAfter in whole seconds, rounded up and never below one. Rounded
// up, because a client obeying 179 for a 179.5 s block comes back early into a second 429; never below one, because
// "Retry-After: 0" reads as "retry now". The body is WriteError's, "rate limited".
func TooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	secs := max(int64(math.Ceil(retryAfter.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	WriteError(w, http.StatusTooManyRequests, "rate limited")
}
