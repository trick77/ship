package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// provider is a minimal OIDC provider: discovery, JWKS, and a token endpoint that checks the PKCE verifier and
// answers with an RS256 id_token it signs itself.
type provider struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	challenge string         // code_challenge from the authorize URL
	claims    map[string]any // extra id_token claims
	nonce     string
	noIDToken bool
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{t: t, key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.srv.URL,
			"authorization_endpoint":                p.srv.URL + "/authorize",
			"token_endpoint":                        p.srv.URL + "/token",
			"jwks_uri":                              p.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "alg": "RS256", "use": "sig",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (p *provider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	defer p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("code") != "good-code" || b64(sum[:]) != p.challenge {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	resp := map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600}
	if !p.noIDToken {
		claims := map[string]any{
			"iss": p.srv.URL, "sub": "user-1", "aud": "client", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "nonce": p.nonce,
		}
		for k, v := range p.claims {
			claims[k] = v
		}
		resp["id_token"] = p.sign(claims)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (p *provider) sign(claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	c, _ := json.Marshal(claims)
	input := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		p.t.Fatal(err)
	}
	return input + "." + b64(sig)
}

// login runs Start, lets the provider see the challenge, and returns the callback request the browser would send.
func (p *provider) login(t *testing.T, o *OIDC, mutate func(q url.Values, nonce *string)) *http.Request {
	t.Helper()
	rec := httptest.NewRecorder()
	o.Start(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("Start = %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	aq := loc.Query()
	if aq.Get("code_challenge_method") != "S256" || aq.Get("code_challenge") == "" {
		t.Fatalf("no PKCE in %s", loc)
	}
	nonce := aq.Get("nonce")
	cq := url.Values{"code": {"good-code"}, "state": {aq.Get("state")}}
	if mutate != nil {
		mutate(cq, &nonce)
	}
	p.mu.Lock()
	p.challenge, p.nonce = aq.Get("code_challenge"), nonce
	p.mu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/callback?"+cq.Encode(), nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func newOIDC(t *testing.T, p *provider, group string) *OIDC {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	o, err := NewOIDC(ctx, OIDCConfig{
		Issuer: p.srv.URL, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://app/callback",
		Group: group, CookiePrefix: "app_", Secure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Only an explicit true vouches for the email: absent, false or a string "true" (a provider bug) are not stated.
func TestOIDCEmailVerifiedOnlyExplicitTrue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		claim any
	}{{"absent", nil}, {"false", false}, {"string", "true"}} {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t)
			p.claims = map[string]any{"email": "jan@example.com"}
			if tt.claim != nil {
				p.claims["email_verified"] = tt.claim
			}
			o := newOIDC(t, p, "")
			c, err := o.Callback(httptest.NewRecorder(), p.login(t, o, nil))
			if tt.name == "string" {
				// A wrong type fails the claims decode: the token is malformed, not unverified.
				if err == nil {
					t.Fatalf("Callback accepted email_verified %q", tt.claim)
				}
				return
			}
			if err != nil {
				t.Fatalf("Callback: %v", err)
			}
			if c.EmailVerified {
				t.Errorf("EmailVerified with email_verified %v", tt.claim)
			}
		})
	}
}

func TestOIDCEndToEnd(t *testing.T) {
	p := newProvider(t)
	p.claims = map[string]any{
		"preferred_username": "jan", "email": "jan@example.com", "given_name": "Jan", "family_name": "Mueller",
		"email_verified": true, "name": "J", "groups": []string{"Admins", "users"},
	}
	o := newOIDC(t, p, "admins")
	rec := httptest.NewRecorder()
	c, err := o.Callback(rec, p.login(t, o, nil))
	if err != nil {
		t.Fatalf("Callback: %v", err)
	}
	if c.Subject != "user-1" || c.Name != "Jan Mueller" || c.Email != "jan@example.com" || c.Username() != "jan" ||
		!c.EmailVerified {
		t.Errorf("claims = %+v", c)
	}
	// Transient cookies are cleared, with the attributes they were set with.
	cleared := 0
	for _, ck := range rec.Result().Cookies() {
		if strings.HasPrefix(ck.Name, "app_oidc_") && ck.MaxAge < 0 && ck.Secure && ck.Path == "/" {
			cleared++
		}
	}
	if cleared != 3 {
		t.Errorf("cleared %d transient cookies, want 3", cleared)
	}
}

func TestOIDCFailures(t *testing.T) {
	p := newProvider(t)
	p.claims = map[string]any{"groups": []string{"users"}}
	for _, tt := range []struct {
		name   string
		group  string
		mutate func(url.Values, *string)
		strip  string // cookie to drop
		noID   bool
		want   error
		substr string
	}{
		{name: "provider error", mutate: func(q url.Values, _ *string) { q.Set("error", "access_denied") }, want: ErrProviderDenied},
		{name: "state mismatch", mutate: func(q url.Values, _ *string) { q.Set("state", "forged") }, want: ErrInvalidState},
		{name: "no state cookie", strip: "app_oidc_state", want: ErrInvalidState},
		{name: "no nonce cookie", strip: "app_oidc_nonce", want: ErrInvalidNonce},
		{name: "no pkce cookie", strip: "app_oidc_pkce", want: ErrInvalidState},
		{name: "nonce replay", mutate: func(_ url.Values, n *string) { *n = "other" }, want: ErrInvalidNonce},
		{name: "bad code", mutate: func(q url.Values, _ *string) { q.Set("code", "stolen") }, substr: "exchange"},
		{name: "no id_token", noID: true, substr: "no id_token"},
		{name: "not in group", group: "admins", want: ErrNotInGroup},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p.mu.Lock()
			p.noIDToken = tt.noID
			p.mu.Unlock()
			o := newOIDC(t, p, tt.group)
			req := p.login(t, o, tt.mutate)
			if tt.strip != "" {
				cookies := req.Cookies()
				req.Header.Del("Cookie")
				for _, c := range cookies {
					if c.Name != tt.strip {
						req.AddCookie(c)
					}
				}
			}
			c, err := o.Callback(httptest.NewRecorder(), req)
			if err == nil {
				t.Fatal("Callback succeeded")
			}
			// Only a refused group returns the identity, for the app's log and its no-access page.
			if errors.Is(err, ErrNotInGroup) {
				if c.Subject != "user-1" || !slices.Equal(c.Groups, []string{"users"}) {
					t.Errorf("ErrNotInGroup claims = %+v", c)
				}
			} else if c.Subject != "" || c.Groups != nil {
				t.Errorf("claims with %v = %+v", err, c)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.substr != "" && !strings.Contains(err.Error(), tt.substr) {
				t.Errorf("err = %v, want %q", err, tt.substr)
			}
		})
	}
}

func TestOIDCForgedToken(t *testing.T) {
	// The JWKS keeps serving the original public key; the token gets signed by another.
	p2 := newProvider(t)
	o2 := newOIDC(t, p2, "")
	req := p2.login(t, o2, nil)
	evil, _ := rsa.GenerateKey(rand.Reader, 2048)
	p2.mu.Lock()
	p2.key = evil
	p2.mu.Unlock()
	if _, err := o2.Callback(httptest.NewRecorder(), req); err == nil || !strings.Contains(err.Error(), "verify") {
		t.Errorf("forged token err = %v", err)
	}
}

func TestNewOIDCDiscoveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := NewOIDC(context.Background(), OIDCConfig{Issuer: srv.URL}); err == nil {
		t.Error("discovery against a 404 succeeded")
	}
}

func TestDisplayName(t *testing.T) {
	for _, tt := range []struct{ name, given, family, want string }{
		{"Full", "Jan", "Mueller", "Jan Mueller"},
		{"Jan Mueller", "Jan", "", "Jan Mueller"},
		{"", "Jan", "", "Jan"},
		{"", "", "", ""},
	} {
		if got := displayName(tt.name, tt.given, tt.family); got != tt.want {
			t.Errorf("displayName(%q,%q,%q) = %q, want %q", tt.name, tt.given, tt.family, got, tt.want)
		}
	}
}
