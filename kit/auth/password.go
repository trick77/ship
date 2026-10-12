package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
)

// ErrBadCredentials is the one answer a failed password login gets: which half was wrong is what a guesser wants.
var ErrBadCredentials = errors.New("bad credentials")

// Verifier decides a password login. The app supplies it: a fixed account (StaticVerifier), a bcrypt hash, a daemon's
// RPC credentials. It must take the same time whatever is wrong, or the timing tells a guesser which half to keep.
type Verifier func(user, pass string) bool

// Check returns the identity for credentials the verifier accepts, ErrBadCredentials otherwise.
func (v Verifier) Check(user, pass string) (Claims, error) {
	if user == "" || v == nil || !v(user, pass) {
		return Claims{}, ErrBadCredentials
	}
	return Claims{Subject: user, PreferredUsername: user, Name: user}, nil
}

// StaticVerifier accepts exactly one user and password. Both halves are compared in constant time over SHA-256
// digests: == leaks a byte at a time, and ConstantTimeCompare on raw values returns early on a length mismatch,
// which leaks the length. An empty configured password accepts nothing, or the username alone would let anyone in.
func StaticVerifier(user, pass string) Verifier {
	wantU, wantP := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
	return func(u, p string) bool {
		gotU, gotP := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
		userOK := subtle.ConstantTimeCompare(gotU[:], wantU[:])
		passOK := subtle.ConstantTimeCompare(gotP[:], wantP[:])
		return pass != "" && userOK&passOK == 1
	}
}
