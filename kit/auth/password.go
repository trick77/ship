package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"

	"golang.org/x/crypto/bcrypt"
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

// HashPassword is the bcrypt hash BcryptVerifier takes, for an app's hash-password command.
func HashPassword(pass string) (string, error) {
	if pass == "" {
		return "", errors.New("empty password")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	return string(h), err
}

// BcryptVerifier accepts one user whose password hashes to hash. It fails at start on a hash that is not bcrypt, so a
// plain password pasted into the env is caught at boot, not at the first login. bcrypt runs whatever the username,
// so a wrong user costs the same time as a wrong password.
func BcryptVerifier(user, hash string) (Verifier, error) {
	if user == "" {
		return nil, errors.New("empty username")
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return nil, errors.New("password hash is not bcrypt")
	}
	wantU := sha256.Sum256([]byte(user))
	h := []byte(hash)
	return func(u, p string) bool {
		gotU := sha256.Sum256([]byte(u))
		userOK := subtle.ConstantTimeCompare(gotU[:], wantU[:]) == 1
		passOK := bcrypt.CompareHashAndPassword(h, []byte(p)) == nil
		return userOK && passOK
	}, nil
}
