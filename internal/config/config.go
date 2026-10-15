// Package config reads ship.yaml, the one file a repo keeps to describe its gate and its release.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/trick77/ship/internal/pins"
)

// uiTest asks vitest for lcov itself: its default reporters write none, and a repo relying on the default must not
// fail on a missing report.
const uiTest = "npm run test -- --coverage --coverage.reporter=lcov --coverage.reporter=text-summary"

// File is the config file name, at the repo root.
const File = "ship.yaml"

// Config is ship.yaml. Every field has a default, so an empty file is a Go-only repo with the family's gates.
type Config struct {
	// Base is the branch the patch gate diffs against and release requires HEAD to be on.
	Base string `yaml:"base"`
	// Lint is the golangci-lint version, run via go run so every machine and CI build the same linter.
	Lint string `yaml:"lint"`
	// Patch is the minimum coverage of the lines a branch adds or changes, in percent.
	Patch float64 `yaml:"patch"`
	Go    Go      `yaml:"go"`
	UI    *UI     `yaml:"ui"`
	// Steps are repo-specific checks (openapi lint, shell tests, e2e), run after lint, before the tests.
	Steps []Step `yaml:"steps"`
	// Postgres is the container check starts for go test, its DSN in SHIP_TEST_DSN: a version ("17", "17.6") for
	// postgres:<v>-alpine, a whole image ref for a postgres with extensions (timescale/timescaledb:2.29.1-pg17), or
	// compose:<service> for that root compose service's image (Dependabot bumps it there: one pin, no drift);
	// empty = none.
	Postgres string `yaml:"postgres"`
	// Shell lists repo-relative shell scripts for shellcheck beyond the ones found by name (*.sh) or shebang.
	Shell  []string `yaml:"shell"`
	Images []Image  `yaml:"images"`
	// TagPrefix goes before release versions: empty for the family's plain 1.2.3, "v" for a Go module.
	TagPrefix string `yaml:"tag_prefix"`
	// Visibility, public or private, is checked against GitHub's by conform; empty = GitHub's.
	Visibility string `yaml:"visibility"`
	// Allow excuses conform rules, each with its why.
	Allow []Allow `yaml:"allow"`
	// Dependabot adds the repo's ignores to ship's dependabot.yaml.
	Dependabot Dependabot `yaml:"dependabot"`
}

// Go configures the Go side of the gate.
type Go struct {
	// Dir is the module directory relative to the repo root.
	Dir string `yaml:"dir"`
	// Floor is the minimum line coverage of the whole module, in percent.
	Floor float64 `yaml:"floor"`
	// Race runs the tests with -race (needs cgo; CGO_ENABLED=1 is set for the test run only).
	Race *bool `yaml:"race"`
	// Coverpkg is passed to go test -coverpkg: default ./... (a package's lines count as covered by any package's
	// tests, every package instrumented), ./internal/... to narrow, "" or null = none. A pointer, set to the
	// default before decoding: a null sets it nil (a string keeps its value on null, the default would win).
	Coverpkg *string `yaml:"coverpkg"`
	// Exclude lists Go path prefixes, relative to the repo root, that neither coverage gate counts. A prefix also
	// matches as a path segment anywhere ("cmd/" excludes backend/cmd/x.go). UI coverage scope is the test
	// runner's own (vitest coverage include/exclude).
	Exclude []string `yaml:"exclude"`
	// Modules are repo-relative dirs of nested Go modules (each its own go.mod), gated like Dir: same commands,
	// the same floor each on its own, their lines in the patch gate. Coverpkg is Dir's alone. A nested go.mod not
	// listed fails check: no module is ever silently ungated. Cleaned like Dir (./kit/auth/ is kit/auth).
	Modules []string `yaml:"modules"`
}

// UI configures the frontend side of the gate.
type UI struct {
	// Dir is the UI's directory relative to the repo root, cleaned (./ui/ is ui).
	Dir   string  `yaml:"dir"`
	Floor float64 `yaml:"floor"`
	// Build runs first in check, so the Go packages embed a fresh dist, never a stale one. The family's
	// `npm run build` is `tsc -b && vite build`: the typecheck.
	Build string `yaml:"build"`
	// Lint is the UI's lint/format check (eslint, prettier --check), run after the Go lint; default npm run lint,
	// the only value conform passes; "" runs none, needs a ui allow.
	Lint string `yaml:"lint"`
	// Test runs the UI tests with coverage and must write Lcov.
	Test string `yaml:"test"`
	// Lcov is the lcov report path relative to the repo root. Its SF: paths are relative to Dir.
	Lcov string `yaml:"lcov"`
	// Embed is the repo-relative dir the Go side embeds the build into (go:embed): emptied but for .gitkeep before
	// the build, so no file of an older build ships.
	Embed string `yaml:"embed"`
}

// Step is one extra check command, run with sh -c.
type Step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	// Dir is relative to the repo root.
	Dir string `yaml:"dir"`
}

// Image is one container image release builds and pushes to ghcr.io.
type Image struct {
	// Suffix is appended to ghcr.io/<owner>/<repo>, e.g. "-dashboard"; empty is the repo's own image.
	Suffix        string   `yaml:"suffix"`
	Target        string   `yaml:"target"`
	Containerfile string   `yaml:"containerfile"`
	Context       string   `yaml:"context"`
	Platforms     []string `yaml:"platforms"`
}

// RaceOn reports whether go test runs with -race.
func (g Go) RaceOn() bool { return g.Race == nil || *g.Race }

// CoverpkgArg is go test's -coverpkg value, "" for none.
func (g Go) CoverpkgArg() string {
	if g.Coverpkg == nil {
		return ""
	}
	return *g.Coverpkg
}

// Load reads root/ship.yaml and fills the defaults. A missing file is an error: a repo without one has no gate.
func Load(root string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(root, File)) //nolint:gosec // the repo's own ship.yaml
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no %s at %s", File, root)
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes ship.yaml, rejecting unknown keys so a typo never silently disables a gate. Defaults are set before
// decoding, so an explicit value, 0 included (`floor: 0` while paying down debt), always wins over the default.
func Parse(b []byte) (*Config, error) {
	coverpkg := "./..."
	c := Config{
		Base:  "master",
		Lint:  pins.Golangci,
		Patch: 75,
		Go:    Go{Dir: ".", Floor: 75, Coverpkg: &coverpkg, Exclude: []string{"cmd/"}},
	}
	// yaml fills an already set pointer in place: a ui: block keeps the defaults it does not override. A bare
	// `ui:` (null) decodes to nil and is put back: naming the key means a UI, never a silently skipped gate.
	// Two passes, not a UnmarshalYAML method: node.Decode would drop KnownFields inside the ui: block.
	uiDefaults := func() *UI {
		return &UI{Dir: "ui", Floor: 75, Build: "npm ci && npm run build", Lint: "npm run lint", Test: uiTest}
	}
	var keys map[string]any
	_ = yaml.Unmarshal(b, &keys) // malformed input fails the strict decode below
	_, hasUI := keys["ui"]
	if hasUI {
		c.UI = uiDefaults()
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	if hasUI && c.UI == nil {
		c.UI = uiDefaults()
	}
	if err := c.defaults(); err != nil {
		return nil, err
	}
	return &c, c.validate()
}

// defaults fills what depends on other values or lives in lists. go.dir, ui.dir and go.modules are cleaned here,
// once (backend, never ./backend/): dependabot directories, the rendered callers, conform's paths and release's -dir
// are spelled from them, every user takes them as is. An escaping dir fails here, quoted as written.
func (c *Config) defaults() error {
	var err error
	if c.Go.Dir, err = cleanDir("go.dir", c.Go.Dir); err != nil {
		return err
	}
	seen := map[string]bool{c.Go.Dir: true}
	for i, m := range c.Go.Modules {
		d, err := cleanDir("go.modules", m)
		if err != nil {
			return err
		}
		if d == "." {
			return fmt.Errorf("%s: go.modules: %q is the repo root, want a dir inside the repo like kit/auth", File, m)
		}
		if seen[d] {
			return fmt.Errorf("%s: go.modules: %q, want a dir inside the repo, not go.dir, listed once", File, m)
		}
		c.Go.Modules[i], seen[d] = d, true
	}
	if c.UI != nil {
		if c.UI.Dir, err = cleanDir("ui.dir", c.UI.Dir); err != nil {
			return err
		}
		if c.UI.Lcov == "" {
			// conform's vite reportsDirectory "../coverage/ui", seen from the repo root.
			c.UI.Lcov = filepath.Join(filepath.Dir(c.UI.Dir), "coverage", "ui", "lcov.info")
		}
	}
	for i := range c.Images {
		im := &c.Images[i]
		if im.Containerfile == "" {
			im.Containerfile = "Containerfile"
		}
		if im.Context == "" {
			im.Context = "."
		}
		if len(im.Platforms) == 0 {
			im.Platforms = []string{"linux/amd64"}
		}
	}
	return nil
}

// cleanDir is dir in clean slash form; an error, quoting dir as written, when it leaves the repo.
func cleanDir(key, dir string) (string, error) {
	c, out := CleanDir(dir)
	if out {
		return "", fmt.Errorf("%s: %s: %q is not a directory inside the repo", File, key, dir)
	}
	return c, nil
}

func (c *Config) validate() error {
	for _, p := range []float64{c.Patch, c.Go.Floor} {
		if p < 0 || p > 100 {
			return fmt.Errorf("%s: percentage %v out of 0-100", File, p)
		}
	}
	if c.UI != nil && (c.UI.Floor < 0 || c.UI.Floor > 100) {
		return fmt.Errorf("%s: ui.floor %v out of 0-100", File, c.UI.Floor)
	}
	if c.UI != nil && c.UI.Embed != "" && !inRepo(c.UI.Embed) {
		return fmt.Errorf("%s: ui.embed: %q is not a directory inside the repo", File, c.UI.Embed)
	}
	for i, s := range c.Steps {
		if s.Name == "" || s.Run == "" {
			return fmt.Errorf("%s: steps[%d] needs name and run", File, i)
		}
	}
	if c.Postgres != "" && !pgVersion.MatchString(c.Postgres) && !pgImage.MatchString(c.Postgres) &&
		!pgCompose.MatchString(c.Postgres) {
		return fmt.Errorf("%s: postgres: %q, want a version like \"17\", an image like "+
			"\"timescale/timescaledb:2.29.1-pg17\" or a compose service like \"compose:db\"", File, c.Postgres)
	}
	for _, s := range c.Shell {
		if !inRepo(s) {
			return fmt.Errorf("%s: shell: %q is not a path inside the repo", File, s)
		}
	}
	if err := c.validateDependabot(); err != nil {
		return err
	}
	return c.validateConform()
}

var (
	pgVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	// pgImage is a tagged or digest-pinned image ref: an untagged one would be :latest, a moving target.
	pgImage = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9._-]+(@sha256:[0-9a-f]{64})?|@sha256:[0-9a-f]{64})$`)
	// pgCompose names a compose service, spelled as compose allows.
	pgCompose = regexp.MustCompile(`^compose:[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// PostgresCompose returns the service a postgres: compose:<service> names; ok false for any other form.
func (c *Config) PostgresCompose() (service string, ok bool) {
	if !pgCompose.MatchString(c.Postgres) {
		return "", false
	}
	return strings.TrimPrefix(c.Postgres, "compose:"), true
}

// PostgresImage is the image check starts for go test: postgres:<v>-alpine for a version, else the ref as written.
// compose:<service> is resolved against the repo (internal/compose), not here.
func (c *Config) PostgresImage() string {
	if pgVersion.MatchString(c.Postgres) {
		return "postgres:" + c.Postgres + "-alpine"
	}
	return c.Postgres
}

// CleanDir is p in clean slash form and whether it leaves the repo, lexically: an absolute path or one through "..".
// Slash semantics on every OS (ship.yaml and Git spell paths with /); filepath.IsAbs still catches a Windows drive
// path (C:\x).
func CleanDir(p string) (c string, escapes bool) {
	c = path.Clean(filepath.ToSlash(p))
	return c, filepath.IsAbs(p) || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../")
}

// CleanPath is p, slash form as written, cleaned: the key repo paths compare by, ./ui//x is ui/x. A backslash is
// part of a name on every OS (ship.yaml spells paths with /), so matching never depends on the OS.
func CleanPath(p string) string { return path.Clean(p) }

// inRepo reports whether p is a repo-relative path below the root: it does not escape and is not the root itself.
func inRepo(p string) bool {
	c, out := CleanDir(p)
	return !out && c != "."
}
