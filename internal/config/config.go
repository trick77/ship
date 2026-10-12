// Package config reads ship.yaml, the one file a repo keeps to describe its gate and its release.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	Steps  []Step  `yaml:"steps"`
	Images []Image `yaml:"images"`
	// TagPrefix goes before release versions: empty for the family's plain 1.2.3, "v" for a Go module.
	TagPrefix string `yaml:"tag_prefix"`
}

// Go configures the Go side of the gate.
type Go struct {
	// Dir is the module directory relative to the repo root.
	Dir string `yaml:"dir"`
	// Floor is the minimum line coverage of the whole module, in percent.
	Floor float64 `yaml:"floor"`
	// Race runs the tests with -race (needs cgo; CGO_ENABLED=1 is set for the test run only).
	Race *bool `yaml:"race"`
	// Coverpkg is passed to go test -coverpkg when set, e.g. ./internal/...
	Coverpkg string `yaml:"coverpkg"`
	// Exclude lists Go path prefixes, relative to the repo root, that neither coverage gate counts. A prefix also
	// matches as a path segment anywhere ("cmd/" excludes backend/cmd/x.go). UI coverage scope is the test
	// runner's own (vitest coverage include/exclude).
	Exclude []string `yaml:"exclude"`
}

// UI configures the frontend side of the gate.
type UI struct {
	Dir   string  `yaml:"dir"`
	Floor float64 `yaml:"floor"`
	// Build runs first in check, so the Go packages embed a fresh dist, never a stale one. The family's
	// `npm run build` is `tsc -b && vite build`: the typecheck.
	Build string `yaml:"build"`
	// Lint is the UI's lint/format check (eslint, prettier --check), run after the Go lint; empty = none.
	Lint string `yaml:"lint"`
	// Test runs the UI tests with coverage and must write Lcov.
	Test string `yaml:"test"`
	// Lcov is the lcov report path relative to the repo root. Its SF: paths are relative to Dir.
	Lcov string `yaml:"lcov"`
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
	c := Config{
		Base:  "master",
		Lint:  pins.Golangci,
		Patch: 75,
		Go:    Go{Dir: ".", Floor: 75, Exclude: []string{"cmd/"}},
	}
	// yaml fills an already set pointer in place: a ui: block keeps the defaults it does not override. A bare
	// `ui:` (null) decodes to nil and is put back: naming the key means a UI, never a silently skipped gate.
	// Two passes, not a UnmarshalYAML method: node.Decode would drop KnownFields inside the ui: block.
	uiDefaults := func() *UI {
		return &UI{Dir: "ui", Floor: 75, Build: "npm ci && npm run build", Test: uiTest}
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
	c.defaults()
	return &c, c.validate()
}

// defaults fills what depends on other values or lives in lists.
func (c *Config) defaults() {
	if c.UI != nil && c.UI.Lcov == "" {
		c.UI.Lcov = filepath.Join(c.UI.Dir, "coverage", "lcov.info")
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
	for i, s := range c.Steps {
		if s.Name == "" || s.Run == "" {
			return fmt.Errorf("%s: steps[%d] needs name and run", File, i)
		}
	}
	return nil
}
