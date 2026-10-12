package httpx

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func corsDo(t *testing.T, opts CORSOptions, method string, hdr map[string]string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	var reached bool
	h := CORS(opts)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(method, "/api", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, reached
}

func TestCORSDefault(t *testing.T) {
	rec, reached := corsDo(t, CORSOptions{}, http.MethodOptions, map[string]string{
		"Origin":                         "https://any.example",
		"Access-Control-Request-Method":  "post",
		"Access-Control-Request-Headers": "content-type, x-api-key,\r\nInjected:1",
	})
	if reached || rec.Code != 200 {
		t.Errorf("preflight reached=%v code=%d", reached, rec.Code)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Methods") != "POST" {
		t.Errorf("preflight grant = %v", h)
	}
	if got := h.Get("Access-Control-Allow-Headers"); got != "Content-Type, X-Api-Key, injected1" {
		t.Errorf("Allow-Headers = %q", got)
	}
	if h.Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials granted by default")
	}
	if !slices.Contains(h.Values("Vary"), "Access-Control-Request-Headers") {
		t.Errorf("Vary = %v", h.Values("Vary"))
	}

	// A method outside the list: answered, nothing granted.
	rec, _ = corsDo(t, CORSOptions{}, http.MethodOptions, map[string]string{
		"Origin": "https://any.example", "Access-Control-Request-Method": "DELETE",
	})
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("DELETE preflight = %d %v", rec.Code, rec.Header())
	}

	// Simple requests reach next; only an Origin-bearing allowed method is granted, all vary on Origin.
	for _, tt := range []struct {
		method, origin, want string
	}{
		{http.MethodGet, "https://any.example", "*"},
		{http.MethodGet, "", ""},
		{http.MethodDelete, "https://any.example", ""},
		{http.MethodOptions, "https://any.example", "*"}, // OPTIONS without a request method is no preflight
	} {
		hdr := map[string]string{}
		if tt.origin != "" {
			hdr["Origin"] = tt.origin
		}
		rec, reached := corsDo(t, CORSOptions{}, tt.method, hdr)
		if !reached || rec.Code != http.StatusNoContent {
			t.Errorf("%s %q not passed through", tt.method, tt.origin)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tt.want {
			t.Errorf("%s %q: Allow-Origin = %q, want %q", tt.method, tt.origin, got, tt.want)
		}
		if rec.Header().Get("Vary") != "Origin" {
			t.Errorf("%s %q: Vary = %v", tt.method, tt.origin, rec.Header().Values("Vary"))
		}
	}
}

func TestCORSRestricted(t *testing.T) {
	opts := CORSOptions{
		Origins:     []string{"https://app.example"},
		Methods:     []string{"get", "put"},
		Headers:     []string{"content-type"},
		Credentials: true,
	}
	pre := func(origin, method, headers string) http.Header {
		rec, reached := corsDo(t, opts, http.MethodOptions, map[string]string{
			"Origin": origin, "Access-Control-Request-Method": method, "Access-Control-Request-Headers": headers,
		})
		if reached {
			t.Error("preflight reached next")
		}
		return rec.Header()
	}
	h := pre("https://APP.example", "PUT", "Content-Type")
	if h.Get("Access-Control-Allow-Origin") != "https://APP.example" || h.Get("Access-Control-Allow-Credentials") != "true" ||
		h.Get("Access-Control-Allow-Methods") != "PUT" || h.Get("Access-Control-Allow-Headers") != "Content-Type" {
		t.Errorf("allowed preflight = %v", h)
	}
	for _, tt := range []struct{ origin, method, headers string }{
		{"https://evil.example", "PUT", ""},
		{"https://app.example", "POST", ""},
		{"https://app.example", "PUT", "Content-Type, Authorization"},
	} {
		if h := pre(tt.origin, tt.method, tt.headers); h.Get("Access-Control-Allow-Origin") != "" ||
			h.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("%+v granted: %v", tt, h)
		}
	}

	rec, _ := corsDo(t, opts, http.MethodGet, map[string]string{"Origin": "https://app.example"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("simple GET = %v", rec.Header())
	}
	rec, _ = corsDo(t, opts, http.MethodGet, map[string]string{"Origin": "https://evil.example"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign origin granted: %v", rec.Header())
	}

	defer func() {
		if recover() == nil {
			t.Error("Credentials without Origins accepted")
		}
	}()
	CORS(CORSOptions{Credentials: true})
}
