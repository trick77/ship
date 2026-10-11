// Package check runs the gate: everything CI checks, in one order, on this machine first.
package check

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/cover"
	"github.com/trick77/ship/internal/gitx"
)

// SkipPatch in a commit message on the branch skips the patch gate (a mass reformat or rename adds no logic, but
// every touched line would count). The floor still applies.
const SkipPatch = "[skip patch-coverage]"

// Run runs the whole gate in root and stops at the first failure.
func Run(root string, c *config.Config) error {
	start := time.Now()
	r := runner{root: root, c: c}
	steps := []struct {
		name string
		fn   func() error
	}{
		{"fetch origin/" + c.Base, func() error { return r.run(root, nil, "git", "fetch", "-q", "origin", c.Base) }},
		{"ui build", r.uiBuild},
		{"gofmt", r.gofmt},
		{"go build", func() error { return r.goCmd("build", "./...") }},
		{"golangci-lint " + c.Lint, r.lint},
		{"ui lint", r.uiLint},
		{"repo steps", r.extra},
		{"go test", r.goTest},
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

type runner struct {
	root string
	c    *config.Config
}

func (r runner) goDir() string { return filepath.Join(r.root, r.c.Go.Dir) }

// sh runs a shell command in dir, output streamed.
func (r runner) sh(dir, command string) error {
	cmd := exec.Command("sh", "-c", command) //nolint:gosec // runs the repo's own ship.yaml commands, by design
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, nil
	cmd.Env = cleanEnv()
	return cmd.Run()
}

// gitRepoVars point git at one repo. git sets them for hooks (GIT_DIR in a linked worktree): passed on, a test's
// `git init` + `git commit` in a temp dir would write into the real repo and its checked-out branch.
var gitRepoVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_QUARANTINE_PATH", "GIT_NAMESPACE"}

// cleanEnv is the environment without gitRepoVars, for everything check runs.
func cleanEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(gitRepoVars, k) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

func (r runner) run(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // go and git with ship's own arguments
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = cleanEnv(env...)
	return cmd.Run()
}

func (r runner) goCmd(args ...string) error { return r.run(r.goDir(), nil, "go", args...) }

func (r runner) uiBuild() error {
	if r.c.UI == nil {
		fmt.Println("(no ui)")
		return nil
	}
	return r.sh(filepath.Join(r.root, r.c.UI.Dir), r.c.UI.Build)
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

func (r runner) lint() error {
	return r.goCmd("run", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@"+r.c.Lint, "run")
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
// recorded against.
func (r runner) goTest() error {
	profile, err := r.profile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(profile), 0o750); err != nil {
		return err
	}
	args := []string{"test", "-count=1", "-covermode=atomic", "-coverprofile=" + profile}
	var env []string
	if r.c.Go.RaceOn() {
		args = append(args, "-race")
		env = append(env, "CGO_ENABLED=1")
	}
	if r.c.Go.Coverpkg != "" {
		args = append(args, "-coverpkg="+r.c.Go.Coverpkg)
	}
	return r.run(r.goDir(), env, "go", append(args, "./...")...)
}

// profile is the Go coverage report path, inside the git dir (per worktree): never an untracked file in the tree,
// which the pre-push hook would refuse as uncommitted.
func (r runner) profile() (string, error) {
	p, err := gitx.Git(r.root, "rev-parse", "--git-path", "ship/go.out")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, p)
	}
	return p, nil
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

// gates runs both floors, then the patch gate over Go and UI lines together.
func (r runner) gates() error {
	goLines, err := r.goLines()
	if err != nil {
		return err
	}
	listed, err := gitx.GitZ(r.root, "ls-files", "-z", "-co", "--exclude-standard")
	if err != nil {
		return err
	}
	files := make(map[string]bool, len(listed))
	for _, f := range listed {
		files[f] = true
	}
	all := cover.Lines{}
	all.Merge(goLines)
	// Every result in one run: a mapping error does not hide the floors or the patch gate.
	errs := []error{cover.Mapped("go", goLines, files), cover.Floor("go", goLines, r.c.Go.Floor)}
	if r.c.UI != nil {
		f, err := os.Open(filepath.Join(r.root, r.c.UI.Lcov))
		if err != nil {
			return fmt.Errorf("ui coverage report: %w (ui.test must write ui.lcov)", err)
		}
		uiLines, err := cover.Lcov(f, r.c.UI.Dir, r.root)
		_ = f.Close()
		if err != nil {
			return err
		}
		if len(uiLines) == 0 {
			return fmt.Errorf("ui coverage report %s holds no lines", r.c.UI.Lcov)
		}
		errs = append(errs, cover.Mapped("ui", uiLines, files), cover.Floor("ui", uiLines, r.c.UI.Floor))
		all.Merge(uiLines)
	}
	errs = append(errs, r.patch(all))
	return errors.Join(errs...)
}

func (r runner) goLines() (cover.Lines, error) {
	modPath, err := modulePath(filepath.Join(r.goDir(), "go.mod"))
	if err != nil {
		return nil, err
	}
	profile, err := r.profile()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(profile) //nolint:gosec // ship's own report in the git dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	l, err := cover.GoProfile(f, modPath, r.c.Go.Dir, r.root)
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
