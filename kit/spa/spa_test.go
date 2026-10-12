package spa

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var built = fstest.MapFS{
	"index.html":           {Data: []byte("<!doctype html>shell")},
	"assets/app-abc.js":    {Data: []byte("console.log(1)")},
	"icon.svg":             {Data: []byte("<svg/>")},
	"site.webmanifest":     {Data: []byte("{}")},
	"sub/nested.txt":       {Data: []byte("n")},
	"sub/Report.HTM":       {Data: []byte("<p>report")},
	"admin.html":           {Data: []byte("<!doctype html>admin")},
	".gitkeep":             {},
	"assets/style-def.css": {Data: []byte("body{}")},
}

var unbuilt = fstest.MapFS{".gitkeep": {}}

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestFallbackMode(t *testing.T) {
	h := Handler(built, Options{})
	for _, tt := range []struct {
		path, wantBody, wantCache string
		wantCode                  int
	}{
		{"/", "shell", "no-cache", 200},
		{"/hosts/3/graphs", "shell", "no-cache", 200},
		{"/index.html", "shell", "no-cache", 200},
		{"/assets/app-abc.js", "console.log(1)", "public, max-age=31536000, immutable", 200},
		{"/icon.svg", "<svg/>", "", 200},
		{"/assets/gone-123.js", "", "", 404},
		{"/api", "", "", 404},
		{"/api/v1/typo", "", "", 404},
		{"/sub", "shell", "no-cache", 200},
	} {
		rec := serve(h, tt.path)
		if rec.Code != tt.wantCode {
			t.Errorf("%s: code %d, want %d", tt.path, rec.Code, tt.wantCode)
			continue
		}
		if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("%s: body %q", tt.path, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); tt.wantCode == 200 && got != tt.wantCache {
			t.Errorf("%s: Cache-Control %q, want %q", tt.path, got, tt.wantCache)
		}
	}
}

func TestAllowlistMode(t *testing.T) {
	h := Handler(built, Options{
		Routes:    func(p string) bool { return p == "/" || p == "/trends" || strings.HasPrefix(p, "/runs/") },
		APIPrefix: "/rpc/",
	})
	for path, want := range map[string]int{
		"/": 200, "/trends": 200, "/runs/abc": 200, "/typo": 404, "/rpc": 404, "/rpc/x": 404, "/api/x": 404,
		"/icon.svg": 200,
	} {
		if rec := serve(h, path); rec.Code != want {
			t.Errorf("%s: code %d, want %d", path, rec.Code, want)
		}
	}
}

func TestUnbuilt(t *testing.T) {
	if Built(unbuilt) || !Built(built) {
		t.Fatal("Built misreports")
	}
	h := Handler(unbuilt, Options{Routes: func(p string) bool { return p == "/" }})
	rec := serve(h, "/")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not built") {
		t.Errorf("/ = %d %q", rec.Code, rec.Body.String())
	}
	if rec := serve(h, "/.gitkeep"); rec.Code != 404 {
		t.Errorf("/.gitkeep = %d, want 404 (no file serving without a build)", rec.Code)
	}
	if rec := serve(h, "/other"); rec.Code != 404 {
		t.Errorf("/other = %d", rec.Code)
	}
	custom := Handler(unbuilt, Options{Placeholder: []byte("custom")})
	if rec := serve(custom, "/x"); rec.Body.String() != "custom" {
		t.Errorf("custom placeholder = %q", rec.Body.String())
	}
}

func TestWebmanifestMIME(t *testing.T) {
	if got := mime.TypeByExtension(".webmanifest"); got != "application/manifest+json" {
		t.Errorf("mime = %q", got)
	}
	rec := serve(Handler(built, Options{}), "/site.webmanifest")
	if ct := rec.Header().Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestFiles(t *testing.T) {
	h := Files(built, "icon.svg", "missing.png")
	if rec := serve(h, "/icon.svg"); rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=86400" {
		t.Errorf("icon = %d %v", rec.Code, rec.Header())
	}
	for _, p := range []string{"/index.html", "/missing.png", "/assets/app-abc.js"} {
		if rec := serve(h, p); rec.Code != 404 {
			t.Errorf("%s = %d, want 404", p, rec.Code)
		}
	}
}

func revalidate(h http.Handler, path, tag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("If-None-Match", tag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestShellETag(t *testing.T) {
	h := Handler(built, Options{})
	rec := serve(h, "/hosts/3")
	tag := rec.Header().Get("ETag")
	if rec.Code != 200 || tag == "" || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("shell = %d %v", rec.Code, rec.Header())
	}
	if rec := revalidate(h, "/", tag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation = %d %q, want 304", rec.Code, rec.Body.String())
	}
	if rec := revalidate(h, "/", `"stale"`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "shell") {
		t.Errorf("stale tag = %d %q", rec.Code, rec.Body.String())
	}
}

func TestShellValidation(t *testing.T) {
	h := Handler(built, Options{})
	tag := serve(h, "/").Header().Get("ETag")
	for _, tt := range []struct {
		name, method string
		header       map[string]string
		want         int
	}{
		{"range ignored", http.MethodGet, map[string]string{"Range": "bytes=0-3"}, 200},
		{"if-match ignored", http.MethodGet, map[string]string{"If-Match": `"other"`}, 200},
		{"post never 304", http.MethodPost, map[string]string{"If-None-Match": tag}, 200},
		{"head 304", http.MethodHead, map[string]string{"If-None-Match": tag}, 304},
		{"list and weak", http.MethodGet, map[string]string{"If-None-Match": `"x", W/` + tag}, 304},
		{"star", http.MethodGet, map[string]string{"If-None-Match": "*"}, 304},
	} {
		req := httptest.NewRequest(tt.method, "/deep", nil)
		for k, v := range tt.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, rec.Code, tt.want)
		}
		if tt.want == 200 && tt.method != http.MethodHead && rec.Body.String() != "<!doctype html>shell" {
			t.Errorf("%s: body %q, want the whole shell", tt.name, rec.Body.String())
		}
	}
}

func TestGate(t *testing.T) {
	h := Handler(built, Options{Gate: func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Cookie") == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return false
		}
		return true
	}})
	for _, p := range []string{"/", "/index.html", "/deep/link", "/admin.html", "/sub/Report.HTM"} {
		if rec := serve(h, p); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" ||
			rec.Header().Get("ETag") != "" {
			t.Errorf("%s anonymous = %d %v", p, rec.Code, rec.Header())
		}
	}
	// Real files and assets stay reachable: the login page needs them.
	for _, p := range []string{"/icon.svg", "/assets/app-abc.js"} {
		if rec := serve(h, p); rec.Code != 200 {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/deep/link", nil)
	req.Header.Set("Cookie", "s=1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "shell") {
		t.Errorf("signed in = %d %q", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/admin.html", nil)
	req.Header.Set("Cookie", "s=1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "admin") {
		t.Errorf("signed-in admin.html = %d %q", rec.Code, rec.Body.String())
	}
}

func TestIndex(t *testing.T) {
	plain := serve(Handler(built, Options{}), "/").Header().Get("ETag")
	h := Handler(built, Options{Index: func(b []byte, r *http.Request) []byte {
		return []byte(strings.Replace(string(b), "shell", "shell base="+r.Header.Get("X-Base"), 1))
	}})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Base", "/tm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	tag := rec.Header().Get("ETag")
	if !strings.Contains(rec.Body.String(), "shell base=/tm") || tag == "" || tag == plain {
		t.Fatalf("rewritten = %q, ETag %q (plain %q)", rec.Body.String(), tag, plain)
	}
	req.Header.Set("If-None-Match", tag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("revalidation = %d, want 304", rec.Code)
	}
	// Another base path is other bytes: the old tag no longer matches.
	req.Header.Set("X-Base", "/other")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("other base = %d, want 200", rec.Code)
	}
}
