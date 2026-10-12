// Package spa serves a built single-page app out of an fs.FS, normally an embed of the Vite dist/ directory.
//
// Real files are served as they are; /assets/ (content-hashed by Vite) is cached forever; app routes get the shell
// (index.html), revalidated on every load (no-cache plus an ETag, so the check is a 304) so a deploy reaches browsers
// that have the old one. /api never falls back:
// a mistyped endpoint must stay a 404, not answer JSON callers with HTML. A missing /assets/ file is a 404 too: a
// stale shell asking for a chunk a redeploy removed must not get HTML posing as a script.
//
// A dist/ holding only .gitkeep (go run, a Go-only CI job) answers app routes with a placeholder page and 503, so
// an unbuilt binary says so instead of serving a directory listing or a blank 200 that a healthcheck reads as fine.
package spa

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Go's MIME table has no .webmanifest and a scratch image has no /etc/mime.types, so without this http.FileServer
// serves the manifest as text/plain, which Chrome ignores.
func init() {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

//go:embed placeholder.html
var defaultPlaceholder []byte

// Options configures Handler. The zero value falls back to the shell for every unknown path outside /api and
// /assets/.
type Options struct {
	// Routes, when set, is the allowlist of paths that get the shell (mirroring the app's client router); anything
	// else is a real 404, so a typo does not look like an empty page. nil falls back for every path.
	Routes func(path string) bool
	// APIPrefix never falls back. Defaults to "/api"; the bare prefix is excluded as well as everything below it.
	APIPrefix string
	// Placeholder is the page an unbuilt dist/ serves on app routes. Defaults to a generic "UI not built" page.
	Placeholder []byte
	// Gate runs before the shell is served, on every app route: false means it has answered the request itself (a
	// redirect to the login page), so an anonymous visitor never gets the app's layout. It also guards every other
	// .html file in dist. nil serves everyone. The other files and /assets/ are not gated: a login page needs them.
	Gate func(w http.ResponseWriter, r *http.Request) bool
	// Index rewrites index.html per request (a base path the bundle cannot infer from its URL). It must not modify
	// its argument. The ETag follows the rewritten bytes.
	Index func(index []byte, r *http.Request) []byte
}

// Handler serves dist, which must hold index.html at its root once built.
func Handler(dist fs.FS, o Options) http.Handler {
	if o.APIPrefix == "" {
		o.APIPrefix = "/api"
	}
	o.APIPrefix = strings.TrimRight(o.APIPrefix, "/")
	if o.Placeholder == nil {
		o.Placeholder = defaultPlaceholder
	}
	route := o.Routes
	if route == nil {
		route = func(string) bool { return true }
	}
	files := http.FileServer(http.FS(dist))
	shell, shellErr := fs.ReadFile(dist, "index.html")
	shellTag := etag(shell)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == o.APIPrefix || strings.HasPrefix(p, o.APIPrefix+"/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(p, "/")
		if shellErr == nil && name != "" && name != "index.html" {
			if st, err := fs.Stat(dist, name); err == nil && !st.IsDir() {
				// Another page of the build (admin.html) is as privileged as the shell.
				if ext := strings.ToLower(path.Ext(name)); (ext == ".html" || ext == ".htm") && o.Gate != nil &&
					!o.Gate(w, r) {
					return
				}
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		if strings.HasPrefix(p, "/assets/") || !route(p) {
			http.NotFound(w, r)
			return
		}
		if o.Gate != nil && !o.Gate(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if shellErr != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(o.Placeholder)
			return
		}
		body, tag := shell, shellTag
		if o.Index != nil {
			body = o.Index(shell, r)
			tag = etag(body)
		}
		w.Header().Set("ETag", tag)
		// Only If-None-Match, by hand: http.ServeContent would also answer Range with a 206 slice of the shell and
		// If-Match with a 412, neither of which a page load means.
		if (r.Method == http.MethodGet || r.Method == http.MethodHead) && noneMatch(r.Header.Get("If-None-Match"), tag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
}

// noneMatch reports whether an If-None-Match header names tag, or is "*" (weak comparison, RFC 9110 13.1.2).
func noneMatch(header, tag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == tag {
			return true
		}
	}
	return false
}

// etag is a strong validator over the content: the embedded shell carries no modification time, so without one a
// browser never gets a 304.
func etag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Built reports whether dist holds a real build rather than only the tracked .gitkeep; tests use it to tell the two
// legitimate states apart.
func Built(dist fs.FS) bool {
	_, err := fs.Stat(dist, "index.html")
	return err == nil
}

// Files serves exactly the named files (FS-relative, no leading slash) and 404s anything else, for assets a
// signed-out browser needs outside an auth gate: the login page's icon, the web manifest (fetched without
// credentials by spec). A fixed list, not a prefix, so dropping a file into public/ cannot widen it. Cached a day:
// the names are not fingerprinted.
func Files(dist fs.FS, names ...string) http.Handler {
	allowed := make(map[string]bool, len(names))
	for _, n := range names {
		allowed[n] = true
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if !allowed[name] {
			http.NotFound(w, r)
			return
		}
		if _, err := fs.Stat(dist, name); err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}
