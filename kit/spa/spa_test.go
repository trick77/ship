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
