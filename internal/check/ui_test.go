package check

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// TestUI_whenRealReactProject_thenGatesWork runs the UI side of check against testdata/ui, a real React +
// TypeScript + vite + vitest project: npm ci, tsc -b + vite build, the lint script, vitest --coverage, and the
// floor and patch gate on the lcov it writes. Needs npm (ship's CI sets up node); skipped without it or with -short.
func TestUI_whenRealReactProject_thenGatesWork(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: no npm run")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyTree(t, filepath.Join("..", "..", "testdata", "ui"), filepath.Join(dir, "ui"))
	git("init", "-q", "-b", "master")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("config", "commit.gpgsign", "false")
	write("go.mod", "module example.com/m\n\ngo 1.24\n")
	write("lib/a.go", "package lib\n\nfunc A() int {\n\treturn 1\n}\n")
	write(".git/ship/go.out", "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 1\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/master", "HEAD")

	c, err := config.Parse([]byte("ui:\n  lint: npm run lint\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := runner{root: dir, c: c}
	for _, s := range []struct {
		name string
		fn   func() error
	}{{"build", r.uiBuild}, {"lint", r.uiLint}, {"test", r.uiTest}, {"gates", r.gates}} {
		if err := s.fn(); err != nil {
			t.Fatalf("clean project, %s: %v", s.name, err)
		}
	}

	// A branch adds an untested exported function to a .tsx file: the UI patch gate names its lines.
	greeting := filepath.Join(dir, "ui", "src", "Greeting.tsx")
	src, err := os.ReadFile(greeting)
	if err != nil {
		t.Fatal(err)
	}
	untested := "\nexport function whisper(s: string): string {\n  const lower = s.toLowerCase();\n  return lower + \"...\";\n}\n"
	write("ui/src/Greeting.tsx", string(src)+untested)
	git("commit", "-qam", "untested")
	if err := r.uiTest(); err != nil {
		t.Fatal(err)
	}
	if err := r.gates(); err == nil || !strings.Contains(err.Error(), "ui/src/Greeting.tsx: missing lines") {
		t.Fatalf("untested .tsx change passed: %v", err)
	}

	// A type error fails the build (tsc -b) and the lint script before any test runs.
	write("ui/src/Broken.ts", "export const n: number = \"not a number\";\n")
	if err := r.uiBuild(); err == nil {
		t.Fatal("type error built")
	}
	if err := r.uiLint(); err == nil {
		t.Fatal("type error linted clean")
	}
}

// copyTree copies the fixture's tracked shape: build output, coverage and node_modules are skipped.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "dist", "coverage":
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
