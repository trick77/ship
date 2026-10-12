package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newSessions(t *testing.T, cfg SessionConfig) *Sessions {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "app_session"
	}
	if cfg.Key == nil {
		cfg.Key = RandomKey()
	}
	s, err := NewSessions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func roundTrip(t *testing.T, s *Sessions, c Claims) (*http.Request, *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := s.Issue(rec, c); err != nil {
		t.Fatal(err)
	}
	ck := rec.Result().Cookies()[0]
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(ck)
	return req, ck
}

func TestNewSessionsValidates(t *testing.T) {
	if _, err := NewSessions(SessionConfig{Key: RandomKey()}); err == nil {
		t.Error("empty name accepted")
	}
	if _, err := NewSessions(SessionConfig{Name: "x", Key: []byte("short")}); err == nil {
		t.Error("short key accepted")
	}
	s := newSessions(t, SessionConfig{})
	if s.cfg.TTL != 24*time.Hour || s.cfg.Path != "/" {
		t.Errorf("defaults = %+v", s.cfg)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := newSessions(t, SessionConfig{Path: "/tm/", Secure: true, TTL: time.Hour})
	want := Claims{Subject: "u", Email: "e", Name: "n", Groups: []string{"g"}}
	req, ck := roundTrip(t, s, want)
	if ck.Path != "/tm/" || !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie attrs = %+v", ck)
	}
	got, ok := s.Read(req)
	if !ok || got.Subject != "u" || got.Groups[0] != "g" {
		t.Errorf("Read = %+v, %v", got, ok)
	}

	// Expired.
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, ok := s.Read(req); ok {
		t.Error("expired session accepted")
	}
}

func TestSessionRejectsTampering(t *testing.T) {
	s := newSessions(t, SessionConfig{})
	_, ck := roundTrip(t, s, Claims{Subject: "u"})
	payload, sig, _ := strings.Cut(ck.Value, ".")
	other := newSessions(t, SessionConfig{})
	for name, value := range map[string]string{
		"no separator":  payload,
		"edited":        payload + "x." + sig,
		"bad base64":    "!!!." + s.sign("!!!"),
		"bad json":      "e30x." + s.sign("e30x"),
		"wrong key":     "",
		"empty payload": "." + s.sign(""),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if name == "wrong key" {
			_, ck2 := roundTrip(t, other, Claims{Subject: "u"})
			req.AddCookie(ck2)
		} else {
			req.AddCookie(&http.Cookie{Name: "app_session", Value: value})
		}
		if _, ok := s.Read(req); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, ok := s.Read(httptest.NewRequest(http.MethodGet, "/", nil)); ok {
		t.Error("no cookie accepted")
	}
}

func TestSessionClearMirrorsIssue(t *testing.T) {
	s := newSessions(t, SessionConfig{Path: "/p/", Secure: true})
	rec := httptest.NewRecorder()
	s.Clear(rec)
	ck := rec.Result().Cookies()[0]
	if ck.Name != "app_session" || ck.MaxAge >= 0 || ck.Path != "/p/" || !ck.Secure || ck.SameSite != http.SameSiteLaxMode {
		t.Errorf("clear cookie = %+v", ck)
	}
}

func TestTrimGroupsKeepsAuthorizingGroup(t *testing.T) {
	var groups []string
	for i := range 40 {
		groups = append(groups, fmt.Sprintf("g%02d", i))
	}
	groups = append(groups, "Admins")
	c := trimGroups(Claims{Groups: groups}, "admins")
	if len(c.Groups) != maxCookieGroups || c.Groups[0] != "Admins" {
		t.Errorf("groups = %v", c.Groups)
	}
	if c := trimGroups(Claims{Groups: groups}, ""); len(c.Groups) != maxCookieGroups || c.Groups[0] != "g00" {
		t.Errorf("no keep: %v", c.Groups)
	}
	short := []string{"a"}
	if c := trimGroups(Claims{Groups: short}, "x"); len(c.Groups) != 1 {
		t.Errorf("short list changed: %v", c.Groups)
	}
	// Survives the cookie round trip and still authorizes.
	s := newSessions(t, SessionConfig{KeepGroup: "admins"})
	req, _ := roundTrip(t, s, Claims{Subject: "u", Groups: groups})
	if got, ok := s.Read(req); !ok || !got.HasGroup("ADMINS") {
		t.Errorf("trimmed session lost the group: %v", got.Groups)
	}
}

func TestRequire(t *testing.T) {
	s := newSessions(t, SessionConfig{})
	var seen Claims
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	member, _ := roundTrip(t, s, Claims{Subject: "u", Groups: []string{"ops"}})
	outsider, _ := roundTrip(t, s, Claims{Subject: "v"})
	anon := httptest.NewRequest(http.MethodGet, "/", nil)

	h := s.Require("ops", nil)(next)
	for name, tt := range map[string]struct {
		req  *http.Request
		want int
	}{"member": {member, 204}, "outsider": {outsider, 401}, "anon": {anon, 401}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, tt.req)
		if rec.Code != tt.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, tt.want)
		}
		if tt.want == 401 && rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: rejection cacheable", name)
		}
	}
	if seen.Subject != "u" {
		t.Errorf("claims not on context: %+v", seen)
	}
	redirect := s.Require("", http.RedirectHandler("/login", http.StatusSeeOther))(next)
	rec := httptest.NewRecorder()
	redirect.ServeHTTP(rec, anon)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("custom deny = %d", rec.Code)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Error("claims on an empty context")
	}
}

func TestDeriveKey(t *testing.T) {
	a, b := DeriveKey("token", "netra-session"), DeriveKey("token", "other")
	if len(a) != 32 || string(a) == string(b) || string(a) != string(DeriveKey("token", "netra-session")) {
		t.Error("DeriveKey not deterministic and label-separated")
	}
}

func TestClaimsHelpers(t *testing.T) {
	if !(Claims{}).HasGroup("") || (Claims{Groups: []string{"a"}}).HasGroup("b") {
		t.Error("HasGroup")
	}
	for _, tt := range []struct {
		c    Claims
		want string
	}{
		{Claims{Subject: "s", Email: "e", PreferredUsername: "p"}, "p"},
		{Claims{Subject: "s", Email: "e"}, "e"},
		{Claims{Subject: "s"}, "s"},
	} {
		if got := tt.c.Username(); got != tt.want {
			t.Errorf("Username(%+v) = %q", tt.c, got)
		}
	}
}

func TestPassword(t *testing.T) {
	v := StaticVerifier("admin", "hunter2")
	if c, err := v.Check("admin", "hunter2"); err != nil || c.Subject != "admin" || c.Name != "admin" {
		t.Errorf("good login = %+v, %v", c, err)
	}
	for _, tt := range [][2]string{{"admin", "wrong"}, {"root", "hunter2"}, {"", ""}, {"admin", ""}} {
		if _, err := v.Check(tt[0], tt[1]); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("Check(%q, %q) = %v", tt[0], tt[1], err)
		}
	}
	if _, err := StaticVerifier("admin", "").Check("admin", ""); err == nil {
		t.Error("empty configured password let the username alone in")
	}
	var nilV Verifier
	if _, err := nilV.Check("a", "b"); err == nil {
		t.Error("nil verifier accepted")
	}
}
