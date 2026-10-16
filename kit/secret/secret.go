// Package secret compares a secret a request carries (a bearer token, a webhook secret, an API key) with the
// configured one without telling the caller which byte, or which length, was wrong.
package secret

import (
	"crypto/sha256"
	"crypto/subtle"
)

// Match reports whether got is want. Both are hashed first: == stops at the first differing byte, and
// subtle.ConstantTimeCompare on the raw values returns at once on a length mismatch, which tells a guesser the
// length; the digests are of one length and compared whole. An empty want matches nothing, so an unset secret never
// lets an empty header in.
func Match(got, want string) bool {
	if want == "" {
		return false
	}
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}
