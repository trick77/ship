// Package spa serves a built single-page app out of an fs.FS, normally an embed of the Vite dist/ directory.
//
// Real files are served as they are; /assets/ (content-hashed by Vite) is cached forever; app routes get the shell
// (index.html), revalidated on every load so a deploy reaches browsers that have the old one. /api never falls back:
// a mistyped endpoint must stay a 404, not answer JSON callers with HTML. A missing /assets/ file is a 404 too: a
// stale shell asking for a chunk a redeploy removed must not get HTML posing as a script.
//
// A dist/ holding only .gitkeep (go run, a Go-only CI job) answers app routes with a placeholder page and 503, so
// an unbuilt binary says so instead of serving a directory listing or a blank 200 that a healthcheck reads as fine.
package spa

import (
	_ "embed"
	"io/fs"
	"mime"
	"net/http"
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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == o.APIPrefix || strings.HasPrefix(p, o.APIPrefix+"/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(p, "/")
		if shellErr == nil && name != "" && name != "index.html" {
			if st, err := fs.Stat(dist, name); err == nil && !st.IsDir() {
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if shellErr != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(o.Placeholder)
			return
		}
		_, _ = w.Write(shell)
	})
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
