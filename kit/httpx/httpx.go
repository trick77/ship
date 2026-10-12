// Package httpx is the HTTP plumbing every service repeats: JSON responses, health probes, the server lifecycle, the
// container healthcheck and the middleware chain (access log with request id, panic recovery, cross-origin
// protection, security headers). net/http only.
package httpx

import (
	"encoding/json"
	"net/http"
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
