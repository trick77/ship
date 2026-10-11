// Package check runs the gate: everything CI checks, in one order, on this machine first.
package check

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/cover"
	"github.com/trick77/ship/internal/gitx"
)

// OutDir holds the coverage reports, under the repo root; repos gitignore it.
const OutDir = ".ship"

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
		{"fetch origin/" + c.Base, func() error { return r.sh(root, "git fetch -q origin "+c.Base) }},
		{"ui build", r.uiBuild},
		{"gofmt", r.gofmt},
		{"go vet", func() error { return r.goCmd("vet", "./...") }},
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
	return cmd.Run()
}

func (r runner) run(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // go and git with ship's own arguments
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), env...)
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

func (r runner) gofmt() error {
	files, err := gitx.GoFiles(r.root)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	cmd := exec.Command("gofmt", append([]string{"-l"}, files...)...) //nolint:gosec // files git lists in the repo
	cmd.Dir = r.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		return fmt.Errorf("not gofmt'd, run gofmt -w:\n%s", s)
	}
	return nil
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
	if err := os.MkdirAll(filepath.Join(r.root, OutDir), 0o750); err != nil {
		return err
	}
	args := []string{"test", "-count=1", "-covermode=atomic", "-coverprofile=" + r.profile()}
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

func (r runner) profile() string { return filepath.Join(r.root, OutDir, "go.out") }

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
	all := cover.Lines{}
	all.Merge(goLines)
	var errs []error
	errs = append(errs, cover.Floor("go", goLines, r.c.Go.Floor))
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
		errs = append(errs, cover.Floor("ui", uiLines, r.c.UI.Floor))
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
	f, err := os.Open(r.profile())
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
	return cover.PatchOf(all.Without(r.c.Go.Exclude), changed).Gate(r.c.Patch)
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
