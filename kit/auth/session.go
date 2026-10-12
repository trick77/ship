package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// maxCookieGroups keeps a useful group list without nearing the ~4KB cookie limit: past it the browser silently
// drops the cookie and the user loops through login with nothing logged.
const maxCookieGroups = 16

// SessionConfig configures Sessions.
type SessionConfig struct {
	// Name is the cookie name, app-prefixed so another app on the same parent domain cannot overwrite it.
	Name string
	// Key signs the cookie, 32 bytes or more. Rotating it signs everyone out: nothing else can revoke a session.
	Key []byte
	// TTL is how long a session lasts; nothing renews it. Default 24h.
	TTL time.Duration
	// Path scopes the cookie, "/" by default; an app under a prefix passes "/prefix/" so siblings are not sent it.
	Path string
	// Secure marks the cookie Secure; see OIDCConfig.Secure.
	Secure bool
	// KeepGroup is the group the app authorizes on; it survives the group-list cap.
	KeepGroup string
}

// Sessions signs and checks session cookies. The claims travel in the cookie, base64url JSON plus an HMAC-SHA256
// over it, so there is no session table.
type Sessions struct {
	cfg SessionConfig
	now func() time.Time
}

type sessionPayload struct {
	Claims  Claims `json:"claims"`
	Expires int64  `json:"exp"`
}

// NewSessions checks the config: a short key or no name is a startup error, not a weak cookie.
func NewSessions(cfg SessionConfig) (*Sessions, error) {
	if cfg.Name == "" {
		return nil, errors.New("session cookie name is empty")
	}
	if len(cfg.Key) < 32 {
		return nil, errors.New("session key shorter than 32 bytes")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	if cfg.Path == "" {
		cfg.Path = "/"
	}
	return &Sessions{cfg: cfg, now: time.Now}, nil
}

// RandomKey is 32 fresh random bytes, for a process started without a configured key: its sessions end with it.
func RandomKey() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // a broken entropy source must not hand out a guessable key
	}
	return b
}

// DeriveKey derives a signing key from an existing secret (netra's admin token) so there is no second secret to
// manage, and rotating the first signs everyone out. label separates this use from any other use of the secret.
func DeriveKey(secret, label string) []byte {
	sum := sha256.Sum256([]byte(label + "\x00" + secret))
	return sum[:]
}

// Issue writes the session cookie for c.
func (s *Sessions) Issue(w http.ResponseWriter, c Claims) error {
	exp := s.now().Add(s.cfg.TTL)
	body, err := json.Marshal(sessionPayload{Claims: trimGroups(c, s.cfg.KeepGroup), Expires: exp.Unix()})
	if err != nil {
		return err
	}
	v := base64.RawURLEncoding.EncodeToString(body)
	http.SetCookie(w, s.cookie(v+"."+s.sign(v), exp))
	return nil
}

// Clear expires the cookie. Every attribute but the value matches Issue: a browser keys a cookie by name, domain and
// path, and a clear that differs (netra once cleared Strict what it set Lax) is not a reliable deletion.
func (s *Sessions) Clear(w http.ResponseWriter) {
	c := s.cookie("", time.Unix(0, 0)) //nolint:gosec // see cookie: same attributes as Issue
	c.MaxAge = -1
	http.SetCookie(w, c)
}

func (s *Sessions) cookie(value string, exp time.Time) *http.Cookie {
	return &http.Cookie{ //nolint:gosec // HttpOnly and SameSite are set; Secure is config-driven so local http works
		Name: s.cfg.Name, Value: value, Path: s.cfg.Path, Expires: exp,
		HttpOnly: true, Secure: s.cfg.Secure,
		// Lax, not Strict: Strict is not sent on the redirect out of the OIDC callback that just set it, so a
		// successful sign-in would land on the login page. Lax still withholds it from cross-site POSTs.
		SameSite: http.SameSiteLaxMode,
	}
}

// Read returns the request's identity: a cookie with a valid signature that has not expired.
func (s *Sessions) Read(r *http.Request) (Claims, bool) {
	ck, err := r.Cookie(s.cfg.Name)
	if err != nil {
		return Claims{}, false
	}
	v, sig, ok := strings.Cut(ck.Value, ".")
	// Constant time: a timing oracle would leak the signature byte by byte.
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(v))) {
		return Claims{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return Claims{}, false
	}
	var p sessionPayload
	if err := json.Unmarshal(body, &p); err != nil || s.now().Unix() >= p.Expires {
		return Claims{}, false
	}
	return p.Claims, true
}

// Require guards next: a request without a valid session, or whose session lacks group ("" = any), is answered by
// deny (nil: a bare 401). The identity is on the request context for next (FromContext). Re-checking the group per
// request, not only at login, means changing the configured group takes effect on live sessions.
func (s *Sessions) Require(group string, deny http.Handler) func(http.Handler) http.Handler {
	if deny == nil {
		deny = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := s.Read(r)
			if !ok || !c.HasGroup(group) {
				// Never cached: the next request may carry a valid session.
				w.Header().Set("Cache-Control", "no-store")
				deny.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), c)))
		})
	}
}

func (s *Sessions) sign(v string) string {
	m := hmac.New(sha256.New, s.cfg.Key)
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// trimGroups caps the group list. The group the app checks must survive the cut, or a member would be rejected:
// keep it first (in the IdP's spelling), then fill up to the cap.
func trimGroups(c Claims, keep string) Claims {
	if len(c.Groups) <= maxCookieGroups {
		return c
	}
	trimmed := make([]string, 0, maxCookieGroups)
	for _, g := range c.Groups {
		if keep != "" && strings.EqualFold(g, keep) {
			trimmed = append(trimmed, g)
			break
		}
	}
	for _, g := range c.Groups {
		if len(trimmed) == maxCookieGroups {
			break
		}
		if keep == "" || !strings.EqualFold(g, keep) {
			trimmed = append(trimmed, g)
		}
	}
	c.Groups = trimmed
	return c
}
