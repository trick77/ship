package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// ETag is a strong validator over b: content without a modification time (an embedded file, a rendered image) has
// nothing else a browser can revalidate with. Quoted, as RFC 9110 has it: http.ServeContent ignores an unquoted one,
// silently, and answers the full body every time.
func ETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// NotModified sets tag as the response's ETag and answers 304 when a GET or HEAD names it in If-None-Match (or sends
// "*"); it returns true then, and the caller returns without a body. Only If-None-Match: http.ServeContent would also
// answer Range and If-Match, which a handler writing its bytes itself does not mean. Set Cache-Control before calling
// it: a 304 carries the header set so far.
func NotModified(w http.ResponseWriter, r *http.Request, tag string) bool {
	w.Header().Set("ETag", tag)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !noneMatch(r.Header.Get("If-None-Match"), tag) {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}

// noneMatch reports whether an If-None-Match header names tag, or is "*" (weak comparison, RFC 9110 13.1.2).
func noneMatch(header, tag string) bool {
	for t := range strings.SplitSeq(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == tag {
			return true
		}
	}
	return false
}
