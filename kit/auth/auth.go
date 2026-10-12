// Package auth is browser sign-in for a service with no user table: OIDC (authorization code with PKCE, state and
// nonce in transient cookies, an optional required group), an HMAC-signed session cookie that carries the identity
// itself, and a password check for deployments without an identity provider.
//
// What is app-specific stays with the app: how a password is verified (Verifier: a constant-time compare, bcrypt,
// the daemon's RPC credentials), bearer and agent tokens, and the pages and JSON a login answers with.
//
// A separate module from kit so kit stays dependency-free; it does not import kit.
package auth

import (
	"context"
	"strings"
)

// Claims is the identity a login yields and a session carries. Subject is the provider's stable id; the rest is
// display material and may be empty.
type Claims struct {
	Subject           string   `json:"sub"`
	PreferredUsername string   `json:"preferred_username,omitempty"`
	Email             string   `json:"email,omitempty"`
	Name              string   `json:"name,omitempty"`
	Groups            []string `json:"groups,omitempty"`
}

// HasGroup reports whether the claims carry group, case-insensitively: IdPs disagree on case and an operator types
// whichever they saw last. An empty group means no group check, which matches everyone.
func (c Claims) HasGroup(group string) bool {
	if group == "" {
		return true
	}
	for _, g := range c.Groups {
		if strings.EqualFold(g, group) {
			return true
		}
	}
	return false
}

// Username is the name to show: the chosen handle, else the email, else the opaque subject, so a UI never renders
// an empty user.
func (c Claims) Username() string {
	switch {
	case c.PreferredUsername != "":
		return c.PreferredUsername
	case c.Email != "":
		return c.Email
	default:
		return c.Subject
	}
}

type ctxKey struct{}

// NewContext returns ctx carrying c; Sessions.Require sets it for the handlers it guards.
func NewContext(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext is the signed-in identity Require put on ctx.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(Claims)
	return c, ok
}
