// Package check runs the gate: everything CI checks, in one order, on this machine first.
package check

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/conform"
	"github.com/trick77/ship/internal/cover"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/pins"
	"github.com/trick77/ship/internal/subject"
)

// PRTitle names the env var the Check workflow puts a pull request's title in: a squash merge makes the title
// master's commit subject, so it is checked with the branch's own.
const PRTitle = "SHIP_PR_TITLE"

// PRFork is "true" in the Check workflow for a pull request from a fork: the squash merge keeps only the title, so
// an outside contributor's branch subjects are not checked, the title alone is.
const PRFork = "SHIP_PR_FORK"

// SkipPatch in a commit message on the branch skips the patch gate (a mass reformat or rename adds no logic, but
// every touched line would count). The floor still applies.
const SkipPatch = "[skip patch-coverage]"

// Run runs the whole gate in root and stops at the first failure.
func Run(root string, c *config.Config) error {
	start := time.Now()
	r := runner{root: root, c: c}
	// The postgres container lives from its step to the end of go test, and never past check, failed or not.
	var pgEnv []string
	stopPG := func() {}
	defer func() { stopPG() }()
	steps := []struct {
		name string
		fn   func() error
	}{
		// First: a drifted copy or a forgotten adoption step fails before anything slow runs.
		{"conform", func() error { return conform.Run(root, c, conform.Options{}) }},
		// ship's own git calls keep git's repo variables (a --git-dir/--work-tree repo needs them); only the
		// tests and steps check runs lose them.
		{"fetch origin/" + c.Base, func() error { return r.runGit(root, "fetch", "-q", "origin", c.Base) }},
		{"commit subjects", r.subjects},
		{"secret scan", r.secrets},
		{"ui embed", r.cleanEmbed},
		{"ui build", r.uiBuild},
		{"npm audit", r.npmAudit},
		{"go modules", r.goModules},
		{"gofmt", r.gofmt},
		{"go mod verify", r.modVerify},
		{"go mod tidy", r.modTidy},
		{"go build", func() error {
			return r.eachModule(func(dir string) error { return r.goIn(dir, "build", "./...") })
		}},
		// After the build, so a compile error reads as go's, not govulncheck's; before lint and tests, so a
		// known-vulnerable dependency fails in seconds, not after the test minutes.
		{"govulncheck " + pins.Govulncheck, r.vuln},
		{"golangci-lint " + c.Lint, r.lint},
		{"ui lint", r.uiLint},
		{"shellcheck", r.shellcheck},
		{"repo steps", r.extra},
		{"postgres", func() (err error) {
			pgEnv, stopPG, err = r.postgres()
			return err
		}},
		{"go test", func() error {
			defer stopPG()
			return r.goTest(pgEnv...)
		}},
		{"ui test", r.uiTest},
		{"coverage", r.gates},
	}
	for _, s := range steps {
		fmt.Printf("== %s ==\n", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
	}
	fmt.Printf("ship: check passed in %s\n", time.Since(start).Round(time.Second))
	return nil
}

// Lint runs golangci-lint alone, at the version check runs: a repo's quick lint while editing, with no version of
// its own to keep in step.
func Lint(root string, c *config.Config) error {
	return runner{root: root, c: c}.lint()
}

type runner struct {
	root string
	c    *config.Config
}

// subjects checks that every commit the branch adds to origin/<base> has a Go-style subject (`area: what`). Merges
// and bots (Dependabot) are skipped: neither is written by hand. The branch's `Release:` lines are checked here
// too: once merged, a typo there would fail every auto release until one is cut by hand.
func (r runner) subjects() error {
	rng := "origin/" + r.c.Base + "..HEAD"
	cs, err := gitx.Commits(r.root, rng)
	if err != nil {
		return err
	}
	messages, err := gitx.Messages(r.root, rng)
	if err != nil {
		return err
	}
	if _, _, err := subject.Release(messages); err != nil {
		return fmt.Errorf("release lines (reword: git rebase -i origin/%s): %w", r.c.Base, err)
	}
	what := "commits"
	if t := os.Getenv(PRTitle); t != "" {
		if os.Getenv(PRFork) == "true" {
			cs, what = nil, "fork PR, title only"
		}
		cs = append(cs, gitx.Commit{Hash: "PR title", Subject: t})
	}
	var bad []string
	n := 0
	for _, c := range cs {
		if strings.HasSuffix(c.Author, "[bot]") {
			continue
		}
		n++
		if err := subject.Check(c.Subject); err != nil {
			bad = append(bad, fmt.Sprintf("  %s %s: %v", c.Hash, c.Subject, err))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%d of %d not Go style `area: what` (reword: git rebase -i origin/%s):\n%s",
			len(bad), n, r.c.Base, strings.Join(bad, "\n"))
	}
	fmt.Printf("ship: subjects ok: %d %s\n", n, what)
	return nil
}

func (r runner) goDir() string { return filepath.Join(r.root, r.c.Go.Dir) }

// sh runs a shell command in dir, output streamed.
func (r runner) sh(dir, command string) error {
	cmd := exec.Command("sh", "-c", command) //nolint:gosec // runs the repo's own ship.yaml commands, by design
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, nil
	cmd.Env = gitx.CleanEnv()
	return cmd.Run()
}

func (r runner) run(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // go and git with ship's own arguments
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = gitx.CleanEnv(env...)
	return cmd.Run()
}

// runGit runs ship's own git call, output streamed, with git's repo variables kept.
func (r runner) runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...) //nolint:gosec // git with ship's own arguments
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// goIn runs go in the module at dir (repo-relative).
func (r runner) goIn(dir string, args ...string) error {
	return r.run(filepath.Join(r.root, dir), nil, "go", args...)
}

// modules is go.dir, then go.modules: every Go module the gate runs on.
func (r runner) modules() []string { return append([]string{r.c.Go.Dir}, r.c.Go.Modules...) }

// nested reports whether dir is one of go.modules, not go.dir.
func (r runner) nested(dir string) bool { return cleanSlash(dir) != cleanSlash(r.c.Go.Dir) }

func cleanSlash(p string) string { return path.Clean(filepath.ToSlash(p)) }

// eachModule runs fn for every module, a nested one under its own heading and named in its error.
func (r runner) eachModule(fn func(dir string) error) error {
	for _, m := range r.modules() {
		if !r.nested(m) {
			if err := fn(m); err != nil {
				return err
			}
			continue
		}
		fmt.Printf("-- %s --\n", m)
		if err := fn(m); err != nil {
			return fmt.Errorf("%s: %w", m, err)
		}
	}
	return nil
}

// goModules fails on a listed module without a go.mod and on a nested go.mod not listed: a module the gate does not
// know is never built, linted or tested, and nothing would say so. Untracked ones count: they are about to be added.
func (r runner) goModules() error {
	listed := map[string]bool{}
	for _, m := range r.c.Go.Modules {
		if !fileExists(filepath.Join(r.root, m, "go.mod")) {
			return fmt.Errorf("go.modules: no go.mod in %s", m)
		}
		listed[cleanSlash(m)] = true
	}
	// A pathspec's * crosses /: */go.mod finds every go.mod below the root.
	files, err := gitx.Files(r.root, "*/go.mod")
	if err != nil {
		return err
	}
	var unlisted []string
	for f := range files {
		d := path.Dir(f)
		if ignoredByGo(f) || d == cleanSlash(r.c.Go.Dir) || listed[d] || !fileExists(filepath.Join(r.root, f)) {
			continue
		}
		unlisted = append(unlisted, d)
	}
	if len(unlisted) > 0 {
		slices.Sort(unlisted)
		return fmt.Errorf("nested Go module %s not gated: list it in go.modules", strings.Join(unlisted, ", "))
	}
	if len(r.c.Go.Modules) == 0 {
		fmt.Println("(none nested)")
		return nil
	}
	fmt.Println("ship: go modules:", strings.Join(r.modules(), ", "))
	return nil
}

func (r runner) uiBuild() error {
	if r.c.UI == nil {
		fmt.Println("(no ui)")
		return nil
	}
	return r.sh(filepath.Join(r.root, r.c.UI.Dir), r.c.UI.Build)
}

// npmAudit fails on a known high or critical vulnerability in what the UI ships (dev dependencies omitted: they
// build, they never reach the browser), as govulncheck does for Go.
func (r runner) npmAudit() error {
	if r.c.UI == nil {
		fmt.Println("(no ui)")
		return nil
	}
	return r.run(filepath.Join(r.root, r.c.UI.Dir), nil, "npm", "audit", "--audit-level=high", "--omit=dev")
}

func (r runner) uiLint() error {
	if r.c.UI == nil || r.c.UI.Lint == "" {
		fmt.Println("(none)")
		return nil
	}
	return r.sh(filepath.Join(r.root, r.c.UI.Dir), r.c.UI.Lint)
}

// gofmt checks the Go files the go tool builds: testdata/, vendor/ and _ or . directories hold fixtures (often
// broken on purpose) the build never compiles. Batched, so a big repo never exceeds the argument limit.
func (r runner) gofmt() error {
	all, err := gitx.GoFiles(r.root)
	if err != nil {
		return err
	}
	var files []string
	for _, f := range all {
		if !ignoredByGo(f) {
			files = append(files, f)
		}
	}
	// The gofmt of the toolchain go.mod selects (GOTOOLCHAIN), as build and lint use, not whichever is on PATH.
	goroot, err := goOutput(r.goDir(), "env", "GOROOT")
	if err != nil {
		return err
	}
	gofmt := filepath.Join(goroot, "bin", "gofmt")
	var bad []string
	for len(files) > 0 {
		n := min(len(files), 500)
		cmd := exec.Command(gofmt, append([]string{"-l"}, files[:n]...)...) //nolint:gosec // files git lists in the repo
		cmd.Dir = r.root
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		if s := strings.TrimSpace(string(out)); s != "" {
			bad = append(bad, s)
		}
		files = files[n:]
	}
	if len(bad) > 0 {
		return fmt.Errorf("not gofmt'd, run gofmt -w:\n%s", strings.Join(bad, "\n"))
	}
	return nil
}

// goOutput runs go in dir and returns trimmed stdout.
func goOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...) //nolint:gosec // go with ship's own arguments
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ignoredByGo reports whether the go tool skips the file: under testdata or vendor, or any path element (the file
// name included) starting with _ or .
func ignoredByGo(path string) bool {
	parts := strings.Split(path, "/")
	for i, d := range parts {
		if strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".") {
			return true
		}
		if i < len(parts)-1 && (d == "testdata" || d == "vendor") {
			return true
		}
	}
	return false
}

// modVerify checks the module cache against go.sum: a dependency changed on disk after its download (a restored CI
// cache, a hand edit) never reaches the build.
func (r runner) modVerify() error {
	return r.eachModule(func(dir string) error { return r.goIn(dir, "mod", "verify") })
}

// modTidy fails on a go.mod or go.sum that go mod tidy would change: a missing or stale requirement builds here
// from the module cache and breaks the next clean build. -diff (go 1.23+) prints the change, writes nothing.
func (r runner) modTidy() error {
	return r.eachModule(func(dir string) error {
		if err := r.goIn(dir, "mod", "tidy", "-diff"); err != nil {
			return fmt.Errorf("%w: go.mod/go.sum not tidy, run go mod tidy in %s", err, dir)
		}
		return nil
	})
}

// vuln fails on a known vulnerability the module's code reaches (govulncheck's call-graph mode: an unreached one in
// a dependency is reported, never fatal).
func (r runner) vuln() error {
	return r.eachModule(func(dir string) error {
		return r.goIn(dir, "run", "golang.org/x/vuln/cmd/govulncheck@"+pins.Govulncheck, "./...")
	})
}

// golangciConfig is the family's golangci-lint config, byte for byte the copy the repos kept before ship owned it:
// one set of linters everywhere, no copy to drift.
//
//go:embed golangci.yaml
var golangciConfig string

// golangciNames are the config files golangci-lint looks up itself, in its working dir and each parent.
var golangciNames = []string{".golangci.yml", ".golangci.yaml", ".golangci.toml", ".golangci.json"}

// lint runs golangci-lint in every module with the repo's own config when one sits between the module and the repo
// root (golangci's own lookup; conform decides whether a repo may keep one), else with the embedded family config.
func (r runner) lint() error {
	return r.eachModule(func(dir string) error {
		args := []string{"run", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@" + r.c.Lint, "run"}
		if own := r.ownGolangci(dir); own != "" {
			fmt.Println("ship: golangci-lint config of the repo:", own)
		} else {
			p, err := r.writeGolangci()
			if err != nil {
				return err
			}
			args = append(args, "--config", p)
		}
		return r.goIn(dir, args...)
	})
}

// ownGolangci is the repo's golangci-lint config golangci would find from the module at dir, "" for none. Above the
// repo root is not looked at: a stray ~/.golangci.yaml must not replace the family's.
func (r runner) ownGolangci(dir string) string {
	root := filepath.Clean(r.root)
	for d := filepath.Join(root, dir); ; d = filepath.Dir(d) {
		for _, n := range golangciNames {
			if p := filepath.Join(d, n); fileExists(p) {
				return p
			}
		}
		if d == root || filepath.Dir(d) == d {
			return ""
		}
	}
}

// writeGolangci writes the embedded config into the git dir (never an untracked file in the tree), with paths
// relative to go.mod: golangci's default, relative to the config file, would print every issue as ../../x.go.
func (r runner) writeGolangci() (string, error) {
	cfg, err := withPathMode(golangciConfig)
	if err != nil {
		return "", err
	}
	p, err := r.gitPath("ship/golangci.yaml")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, []byte(cfg), 0o600)
}

// withPathMode adds relative-path-mode: gomod to the config's run: section, leaving the embed itself byte-identical
// to the family's copy.
func withPathMode(cfg string) (string, error) {
	out := strings.Replace(cfg, "\nrun:\n", "\nrun:\n  relative-path-mode: gomod\n", 1)
	if out == cfg {
		return "", errors.New("golangci config has no run: section to set relative-path-mode in")
	}
	return out, nil
}

// gitPath is p inside the git dir (per worktree), absolute.
func (r runner) gitPath(p string) (string, error) {
	p, err := gitx.Git(r.root, "rev-parse", "--git-path", p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, p)
	}
	return p, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (r runner) extra() error {
	if len(r.c.Steps) == 0 {
		fmt.Println("(none)")
	}
	for _, s := range r.c.Steps {
		fmt.Printf("-- %s --\n", s.Name)
		if err := r.sh(filepath.Join(r.root, s.Dir), s.Run); err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	return nil
}

// goTest always runs uncached (-count=1): a cached result carries the coverage counters of the file set it was
// recorded against. env goes to the tests on top of ship's own (the postgres DSN).
// go.coverpkg is go.dir's alone: its patterns (./internal/...) are that module's paths.
func (r runner) goTest(env ...string) error {
	if r.c.Go.RaceOn() {
		env = append(env, "CGO_ENABLED=1")
	}
	return r.eachModule(func(dir string) error {
		profile, err := r.profile(dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(profile), 0o750); err != nil {
			return err
		}
		args := []string{"test", "-count=1", "-covermode=atomic", "-coverprofile=" + profile}
		if r.c.Go.RaceOn() {
			args = append(args, "-race")
		}
		if r.c.Go.Coverpkg != "" && !r.nested(dir) {
			args = append(args, "-coverpkg="+r.c.Go.Coverpkg)
		}
		return r.run(filepath.Join(r.root, dir), env, "go", append(args, "./...")...)
	})
}

// profile is the module's Go coverage report path, inside the git dir (per worktree): never an untracked file in the
// tree, which the pre-push hook would refuse as uncommitted. A nested module's sits under ship/modules/<dir>/.
func (r runner) profile(dir string) (string, error) {
	if r.nested(dir) {
		return r.gitPath("ship/modules/" + cleanSlash(dir) + "/go.out")
	}
	return r.gitPath("ship/go.out")
}

func (r runner) uiTest() error {
	if r.c.UI == nil {
		fmt.Println("(no ui)")
		return nil
	}
	// A stale report must never pass for this run's.
	_ = os.Remove(filepath.Join(r.root, r.c.UI.Lcov))
	return r.sh(filepath.Join(r.root, r.c.UI.Dir), r.c.UI.Test)
}

// gates runs both floors, then the patch gate over Go and UI lines together. Every result in one run: a broken
// report does not hide the other side's floor. The patch gate needs every report sound, so with one broken it is
// skipped and says so, never prints a pass computed over missing or mis-mapped lines.
func (r runner) gates() error {
	var errs []error
	sound := true
	// Without git's listing no path can be judged, but the floors still are.
	files, err := gitx.Files(r.root)
	if err != nil {
		errs, sound = append(errs, err), false
	}
	all := cover.Lines{}
	side := func(name, hint string, l cover.Lines, err error, floor float64) {
		if err == nil && files != nil {
			err = cover.Mapped(name, hint, l, files, r.root)
		}
		if err != nil {
			errs, sound = append(errs, err), false
			return
		}
		errs = append(errs, cover.Floor(name, l, floor))
		all.Merge(l)
	}
	// A floor per module, not one over all: each is released and imported on its own, and the root's lines must not
	// carry a thin nested library.
	for _, m := range r.modules() {
		name, hint := "go", "check go.dir and the module path in go.mod; a replace'd module in go.coverpkg lives "+
			"outside go.dir: narrow go.coverpkg"
		if r.nested(m) {
			name, hint = "go "+m, "check go.modules and the module path in "+m+"/go.mod"
		}
		l, err := r.goLines(m)
		side(name, hint, l, err, r.c.Go.Floor)
	}
	if r.c.UI != nil {
		uiLines, err := r.uiLines()
		side("ui", "check ui.dir, ui.lcov and the test runner's root (vitest root, coverage.reportsDirectory)",
			uiLines, err, r.c.UI.Floor)
	}
	if !sound {
		fmt.Println("ship: patch coverage not run: a coverage report is broken, see the error below")
		return errors.Join(errs...)
	}
	errs = append(errs, r.patch(all))
	return errors.Join(errs...)
}

func (r runner) uiLines() (cover.Lines, error) {
	f, err := os.Open(filepath.Join(r.root, r.c.UI.Lcov))
	if err != nil {
		return nil, fmt.Errorf("ui coverage report: %w (ui.test must write ui.lcov)", err)
	}
	defer func() { _ = f.Close() }()
	l, err := cover.Lcov(f, r.c.UI.Dir, r.root)
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, fmt.Errorf("ui coverage report %s holds no lines", r.c.UI.Lcov)
	}
	return l, nil
}

func (r runner) goLines(dir string) (cover.Lines, error) {
	modPath, err := modulePath(filepath.Join(r.root, dir, "go.mod"))
	if err != nil {
		return nil, err
	}
	profile, err := r.profile(dir)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(profile) //nolint:gosec // ship's own report in the git dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	l, err := cover.GoProfile(f, modPath, dir, r.root)
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, fmt.Errorf("coverprofile holds no lines of module %s", modPath)
	}
	return l.Without(r.c.Go.Exclude), nil
}

func (r runner) patch(all cover.Lines) error {
	ref := "origin/" + r.c.Base
	base, err := gitx.MergeBase(r.root, ref)
	if err != nil {
		return err
	}
	if skip, err := gitx.SkipMarker(r.root, base, SkipPatch); err != nil {
		return err
	} else if skip {
		fmt.Printf("ship: patch coverage SKIPPED: a commit on this branch carries %s (floors still apply)\n", SkipPatch)
		return nil
	}
	changed, err := gitx.Changed(r.root, base)
	if err != nil {
		return err
	}
	return cover.PatchOf(all, changed).Gate(r.c.Patch)
}

// modulePath reads the module line of go.mod.
func modulePath(gomod string) (string, error) {
	f, err := os.Open(gomod) //nolint:gosec // the repo's go.mod
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(p), `"`), nil
		}
	}
	return "", fmt.Errorf("%s: no module line", gomod)
}
