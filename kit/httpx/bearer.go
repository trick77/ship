package httpx

import (
	"net/http"
	"strings"
)

// Bearer is the token of the request's Authorization: Bearer header, trimmed; ok is false without the header, for
// another scheme, an empty token or one with whitespace inside (RFC 9110's token68 has none). The scheme is matched
// case-insensitively, as RFC 9110 has it. Compare the token with secret.Match: == and subtle.ConstantTimeCompare on
// the raw values both leak.
func Bearer(r *http.Request) (token string, ok bool) {
	scheme, rest, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(rest)
	if token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}
