package auth

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Errors of a callback. No error tells the browser which check failed; the app logs it and shows one message.
// ErrNotInGroup is the exception to "one message": the identity is proven, only access is refused, so Callback
// returns the claims with it and the app can log who was turned away and say "no access" rather than "sign-in
// failed".
var (
	ErrInvalidState   = errors.New("invalid oidc state")
	ErrInvalidNonce   = errors.New("invalid oidc nonce")
	ErrNotInGroup     = errors.New("user not in the required group")
	ErrProviderDenied = errors.New("provider returned an error")
)

// transientTTL bounds a half-finished login: long enough for a password prompt and a second factor.
const transientTTL = 10 * time.Minute

// OIDCConfig configures the flow.
type OIDCConfig struct {
	Issuer, ClientID, ClientSecret, RedirectURL string
	// Scopes defaults to openid, profile, email, groups. Authelia emits the groups claim only when the scope is
	// asked for; an app without a group check (netra) may leave groups out.
	Scopes []string
	// Group, when set, is a group the user must be in.
	Group string
	// CookiePrefix names the transient cookies (<prefix>oidc_state, ...), so two apps on one parent domain do not
	// overwrite each other's half-finished logins.
	CookiePrefix string
	// CookiePath scopes the transient cookies, "/" by default; an app under a path prefix passes "/prefix/".
	CookiePath string
	// Secure marks the cookies Secure: true behind TLS, false only for local plain-http runs, where a browser
	// drops a Secure cookie and sign-in loops.
	Secure bool
}

// Backend is the seam over oauth2 and go-oidc: everything that talks to the network, so the state, nonce and group
// checks run end to end in a test without a provider.
type Backend interface {
	AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string
	Exchange(ctx context.Context, code string, opts ...oauth2.AuthCodeOption) (*oauth2.Token, error)
	// Verify checks the ID token and returns its claims and nonce.
	Verify(ctx context.Context, tok *oauth2.Token) (Claims, string, error)
}

// OIDC drives the authorization code flow.
type OIDC struct {
	backend Backend
	cfg     OIDCConfig
}

// NewOIDC reads the provider's discovery document. A network call made once at start, under ctx's deadline: a
// provider that cannot be reached should stop the app claiming it supports sign-in, not fail the first person who
// tries.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	p, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover oidc provider: %w", err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email", "groups"}
	}
	b := providerBackend{
		cfg: oauth2.Config{
			ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
			Endpoint: p.Endpoint(), Scopes: scopes,
		},
		verifier: p.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}
	return NewOIDCWithBackend(b, cfg), nil
}

// NewOIDCWithBackend builds the flow around b; tests pass a fake.
func NewOIDCWithBackend(b Backend, cfg OIDCConfig) *OIDC {
	if cfg.CookiePath == "" {
		cfg.CookiePath = "/"
	}
	return &OIDC{backend: b, cfg: cfg}
}

func (o *OIDC) cookieName(n string) string { return o.cfg.CookiePrefix + "oidc_" + n }

// Start redirects to the provider, remembering state, nonce and the PKCE verifier in cookies so the callback can
// prove the answer belongs to this browser's request. PKCE on top of the client secret: a code lifted off the
// redirect is useless without the verifier, which never leaves the cookie.
func (o *OIDC) Start(w http.ResponseWriter, r *http.Request) {
	state, nonce, verifier := randomToken(), randomToken(), oauth2.GenerateVerifier()
	exp := time.Now().Add(transientTTL)
	http.SetCookie(w, o.transient("state", state, exp))
	http.SetCookie(w, o.transient("nonce", nonce, exp))
	http.SetCookie(w, o.transient("pkce", verifier, exp))
	http.Redirect(w, r, o.backend.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)),
		http.StatusFound)
}

// Callback checks the provider's answer, state, code exchange, token, nonce and group, and returns the identity. The
// transient cookies are cleared whatever the outcome: single-use, or a captured callback URL replays while they live.
//
// On ErrNotInGroup the claims come back too: verified, every check but the group passed, for the log and the
// "no access" page, never for a session. Every other error returns zero Claims.
func (o *OIDC) Callback(w http.ResponseWriter, r *http.Request) (Claims, error) {
	for _, n := range []string{"state", "nonce", "pkce"} {
		http.SetCookie(w, o.transient(n, "", time.Unix(0, 0)))
	}
	q := r.URL.Query()
	// A provider that refuses is not a bug to debug: the user was denied, or cancelled.
	if e := q.Get("error"); e != "" {
		return Claims{}, fmt.Errorf("%w: %s", ErrProviderDenied, e)
	}
	sc, err := r.Cookie(o.cookieName("state"))
	// Constant time: the state is a secret for the length of one login.
	if err != nil || sc.Value == "" || subtle.ConstantTimeCompare([]byte(sc.Value), []byte(q.Get("state"))) != 1 {
		return Claims{}, ErrInvalidState
	}
	nc, err := r.Cookie(o.cookieName("nonce"))
	if err != nil || nc.Value == "" {
		return Claims{}, ErrInvalidNonce
	}
	pc, err := r.Cookie(o.cookieName("pkce"))
	if err != nil || pc.Value == "" {
		return Claims{}, ErrInvalidState
	}
	tok, err := o.backend.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(pc.Value))
	if err != nil {
		return Claims{}, fmt.Errorf("exchange oidc code: %w", err)
	}
	claims, nonce, err := o.backend.Verify(r.Context(), tok)
	if err != nil {
		return Claims{}, fmt.Errorf("verify oidc token: %w", err)
	}
	// After verification, never before: an unverified token's nonce proves nothing. An absent nonce fails: a
	// provider that drops the claim must not turn the replay check off.
	if nonce == "" || subtle.ConstantTimeCompare([]byte(nonce), []byte(nc.Value)) != 1 {
		return Claims{}, ErrInvalidNonce
	}
	if !claims.HasGroup(o.cfg.Group) {
		return claims, ErrNotInGroup
	}
	return claims, nil
}

func (o *OIDC) transient(name, value string, exp time.Time) *http.Cookie {
	c := &http.Cookie{ //nolint:gosec // HttpOnly and SameSite are set; Secure is config-driven so local http works
		Name: o.cookieName(name), Value: value, Path: o.cfg.CookiePath, Expires: exp,
		HttpOnly: true, Secure: o.cfg.Secure,
		// Lax, not Strict: the callback is a top-level navigation from the provider's origin, and Strict would
		// withhold these exactly then.
		SameSite: http.SameSiteLaxMode,
	}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}

type providerBackend struct {
	cfg      oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func (b providerBackend) AuthCodeURL(state string, opts ...oauth2.AuthCodeOption) string {
	return b.cfg.AuthCodeURL(state, opts...)
}

func (b providerBackend) Exchange(ctx context.Context, code string, opts ...oauth2.AuthCodeOption) (*oauth2.Token, error) {
	return b.cfg.Exchange(ctx, code, opts...)
}

// Verify checks signature, audience and expiry against the provider's JWKS. The access token is discarded: an app
// with no upstream API to call for the user has no use for it.
func (b providerBackend) Verify(ctx context.Context, tok *oauth2.Token) (Claims, string, error) {
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Claims{}, "", errors.New("token response carried no id_token")
	}
	id, err := b.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, "", err
	}
	var c struct {
		PreferredUsername string   `json:"preferred_username"`
		Email             string   `json:"email"`
		EmailVerified     *bool    `json:"email_verified"`
		Name              string   `json:"name"`
		GivenName         string   `json:"given_name"`
		FamilyName        string   `json:"family_name"`
		Groups            []string `json:"groups"`
	}
	if err := id.Claims(&c); err != nil {
		return Claims{}, "", err
	}
	return Claims{
		Subject: id.Subject, PreferredUsername: c.PreferredUsername, Email: c.Email,
		EmailVerified: c.EmailVerified != nil && *c.EmailVerified,
		Name:          displayName(c.Name, c.GivenName, c.FamilyName), Groups: c.Groups,
	}, id.Nonce, nil
}

// displayName prefers given + family name so the full name shows, but only when both are present: composing from
// one half would turn an IdP's "Jan Mueller" into "Jan".
func displayName(name, given, family string) string {
	if given != "" && family != "" {
		return given + " " + family
	}
	if name != "" {
		return name
	}
	return strings.TrimSpace(given + " " + family)
}

// randomToken is 256 random bits. crypto/rand failing is not recoverable and must never degrade into a predictable
// state or nonce.
func randomToken() string { return base64.RawURLEncoding.EncodeToString(RandomKey()) }
