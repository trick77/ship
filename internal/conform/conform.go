// Package conform checks a repo against ship's canon: the files ship owns, the workflow callers, dependabot, the
// Containerfile, the UI's scripts, go.mod. Every divergence an audit of the consumer repos found was a copied file
// that drifted or an adoption step forgotten; conform runs first in every check, so neither survives a push. Hard
// fail, no warn mode: an exception is a ship.yaml allow entry with its why.
package conform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ship/internal/compose"
	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/scaffold"
)

// Rule names, as ship.yaml allow entries spell them.
const (
	Scaffold      = "scaffold"
	Workflows     = "workflows"
	Visibility    = "visibility"
	Dependabot    = "dependabot"
	Containerfile = "containerfile"
	Distroless    = "distroless"
	Dockerignore  = "dockerignore"
	UI            = "ui"
	Golangci      = "golangci"
	Gomod         = "gomod"
	Kit           = "kit"
	Handrolled    = "handrolled"
	Agents        = "agents"
	Postgres      = "postgres"
)

// Options are conform's outside world, injectable for tests. Zero values are the real one.
type Options struct {
	Out io.Writer
	// HTTP asks GitHub for the repo's visibility; API is its base URL.
	HTTP *http.Client
	API  string
	// Getenv reads GITHUB_ACTIONS and GITHUB_EVENT_PATH.
	Getenv func(string) string
	Now    func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if o.API == "" {
		o.API = "https://api.github.com"
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// finding is one divergence. Image is the image suffix for per-image rules, nil otherwise.
type finding struct {
	what, file string
	image      *string
}

// result is a rule's verdict: findings, or skip (the rule does not apply), or an ok with a note.
type result struct {
	findings []finding
	skip     string
	note     string
}

type rule struct {
	name string
	fn   func(*env) result
}

func rules() []rule {
	return []rule{
		{Scaffold, (*env).scaffold},
		{Workflows, (*env).workflows},
		{Visibility, (*env).visibilityRule},
		{Dependabot, (*env).dependabot},
		{Containerfile, (*env).containerfile},
		{Distroless, (*env).distroless},
		{Dockerignore, (*env).dockerignore},
		{UI, (*env).ui},
		{Golangci, (*env).golangci},
		{Gomod, (*env).gomod},
		{Kit, (*env).kit},
		{Handrolled, (*env).handrolled},
		{Agents, (*env).agents},
		{Postgres, (*env).postgres},
	}
}

// env is one conform run's view of the repo.
type env struct {
	root  string
	c     *config.Config
	o     Options
	owner string
	repo  string
	// vis is GitHub's visibility (or the event's); visErr why it is unknown.
	vis    string
	visErr error
	// visNote: the lookup failed and ship.yaml's visibility stood in.
	visNote string
	files   []scaffold.File
	git     map[string]bool
	// renderErr: the owned files could not be rendered at all.
	renderErr error
	// compose: composePins, read once for the render and the dependabot rule alike.
	compose bool
}

func newEnv(root string, c *config.Config, o Options) *env {
	e := &env{root: root, c: c, o: o.withDefaults()}
	e.owner, e.repo = origin(root)
	e.vis, e.visErr = e.visibility()
	// Offline or rate-limited: the declared visibility stands, the next successful lookup still checks it.
	if e.visErr != nil && c.Visibility != "" {
		e.visNote = fmt.Sprintf("GitHub lookup failed (%v); ship.yaml's visibility: %s used", e.visErr, c.Visibility)
		e.vis, e.visErr = c.Visibility, nil
	}
	e.compose = e.composePins()
	e.render()
	return e
}

// render renders the owned files with the visibility ship.yaml names, else GitHub's.
func (e *env) render() {
	f := scaffold.Facts{Repo: e.repo, Visibility: e.c.Visibility, DockerDirs: e.dockerDirs(),
		Compose: e.compose, NodeVersion: e.nodeVersion()}
	if f.Visibility == "" {
		f.Visibility = e.vis
	}
	e.files, e.renderErr = scaffold.Files(e.root, e.c, f)
}

// Run checks root against the canon and prints one line per rule (ok, FAIL, allowed, skip). Any FAIL is an error.
func Run(root string, c *config.Config, o Options) error {
	e := newEnv(root, c, o)
	return e.report()
}

// Init writes the owned files that differ from ship's templates. diff: print what would change, write nothing,
// error when anything differs.
func Init(root string, c *config.Config, o Options, diff bool) error {
	e := newEnv(root, c, o)
	if e.renderErr != nil {
		return e.renderErr
	}
	if diff {
		if scaffold.PrintDiff(e.o.Out, e.files) {
			return errors.New("init -diff: owned files differ from ship's templates (ship init writes them)")
		}
		return nil
	}
	wrote, err := scaffold.Write(root, e.files)
	for _, p := range wrote {
		_, _ = fmt.Fprintln(e.o.Out, "ship: wrote", p)
	}
	if err != nil {
		return err
	}
	var manual []string
	for _, f := range e.files {
		if f.Manual != "" {
			manual = append(manual, fmt.Sprintf("  %s: %s", f.Path, f.Manual))
		}
	}
	if len(manual) > 0 {
		return fmt.Errorf("init: by hand:\n%s", strings.Join(manual, "\n"))
	}
	return nil
}

// Fix runs init, then conform: what is left is the manual part.
func Fix(root string, c *config.Config, o Options) error {
	o = o.withDefaults()
	if err := Init(root, c, o, false); err != nil {
		_, _ = fmt.Fprintln(o.Out, "ship:", err)
	}
	return Run(root, c, o)
}

func (e *env) report() error {
	known := map[string]bool{}
	fails := 0
	for _, r := range rules() {
		known[r.name] = true
		res := r.fn(e)
		switch {
		case res.skip != "":
			_, _ = fmt.Fprintf(e.o.Out, "skip %s: %s\n", r.name, res.skip)
			continue
		case len(res.findings) == 0 && res.note != "":
			_, _ = fmt.Fprintf(e.o.Out, "ok %s: %s\n", r.name, res.note)
			continue
		case len(res.findings) == 0:
			_, _ = fmt.Fprintf(e.o.Out, "ok %s\n", r.name)
			continue
		}
		var seen []string
		for _, f := range res.findings {
			if a, ok := e.c.Allowed(r.name, f.image, f.file); ok {
				if !slices.Contains(seen, a.Why) {
					seen = append(seen, a.Why)
					_, _ = fmt.Fprintf(e.o.Out, "allowed %s: %s\n", r.name, a.Why)
				}
				continue
			}
			fails++
			_, _ = fmt.Fprintf(e.o.Out, "FAIL %s: %s (%s)\n", r.name, f.what, f.file)
		}
	}
	for _, a := range e.c.Allow {
		if !known[a.Rule] {
			fails++
			_, _ = fmt.Fprintf(e.o.Out, "FAIL allow: %q names no conform rule (%s)\n", a.Rule, config.File)
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d failed (ship conform -fix writes the files ship owns; an exception is a "+
			"ship.yaml allow entry with its why)", fails)
	}
	return nil
}

// origin is the GitHub owner and repo of the origin remote; empty when there is none or it is not GitHub.
func origin(root string) (owner, repo string) {
	u, err := gitx.Git(root, "remote", "get-url", "origin")
	if err != nil {
		return "", ""
	}
	return parseOrigin(u)
}

func parseOrigin(u string) (owner, repo string) {
	u = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git")
	for _, p := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:",
		"git://github.com/"} {
		if rest, ok := strings.CutPrefix(u, p); ok {
			if o, r, ok := strings.Cut(rest, "/"); ok && o != "" && r != "" && !strings.Contains(r, "/") {
				return o, r
			}
		}
	}
	return "", ""
}

// read returns a repo file, nil when it does not exist.
func (e *env) read(p string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(p))) //nolint:gosec // the repo's own files
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func (e *env) exists(p string) bool {
	_, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(p)))
	return err == nil
}

// ownDiff reports whether ship.yaml allows owned file p to differ from its template. Byte-equal, the scaffold rule
// already holds everything a rule reads in p: the rule checks p only here, its one guard then, and never doubles a
// scaffold finding.
func (e *env) ownDiff(p string) bool {
	_, ok := e.c.Allowed(Scaffold, nil, p)
	return ok
}

func (e *env) goPath(p string) string { return path.Join(filepath.ToSlash(e.c.Go.Dir), p) }

func (e *env) uiPath(p string) string { return path.Join(filepath.ToSlash(e.c.UI.Dir), p) }

// packageJSON is the UI's package.json, the parts conform reads.
type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func (e *env) packageJSON() (*packageJSON, error) {
	if e.c.UI == nil {
		return nil, nil
	}
	b, err := e.read(e.uiPath("package.json"))
	if err != nil || b == nil {
		return nil, err
	}
	var p packageJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", e.uiPath("package.json"), err)
	}
	return &p, nil
}

// dockerDirs are the dependabot directories of the images' Containerfiles that exist.
func (e *env) dockerDirs() []string {
	var dirs []string
	for _, im := range e.c.Images {
		if !e.exists(im.Containerfile) {
			continue
		}
		if d := path.Join("/", path.Dir(filepath.ToSlash(im.Containerfile))); !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

var composeImg = regexp.MustCompile(`(?m)^\s*image:\s*["']?([^\s"'#]+)`)

// composePins: a compose file at the root (tracked or untracked, not ignored) names an image by a version tag or a
// digest. An untagged, ${VAR} or digit-free tag (latest, alpine, stable, lts) is not one Dependabot could bump.
func (e *env) composePins() bool {
	for _, f := range compose.Files(e.listed()) {
		b, err := e.read(f)
		if err != nil {
			continue
		}
		for _, m := range composeImg.FindAllSubmatch(b, -1) {
			if strings.Contains(string(m[1]), "$") {
				continue
			}
			ref, digest, _ := strings.Cut(string(m[1]), "@")
			// A digest is a pin Dependabot moves, tag or not.
			if strings.HasPrefix(digest, "sha256:") {
				return true
			}
			// A version names a number (17-alpine, 2.29.1-pg17, 4.1.3-r0-ls360); a channel name does not.
			if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') &&
				strings.ContainsAny(ref[i+1:], "0123456789") {
				return true
			}
		}
	}
	return false
}
