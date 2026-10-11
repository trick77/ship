package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// fixture is a repo with origin/master at the first commit and a branch commit adding lib/b.go on top, a Go
// profile in .ship/go.out and an lcov report, so the gates run without go test or npm.
func fixture(t *testing.T, cfg string, profile, lcov string) (string, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "master")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("config", "commit.gpgsign", "false")
	write("go.mod", "module example.com/m\n\ngo 1.24\n")
	write(".gitignore", ".ship/\n")
	write("lib/a.go", "package lib\n\nfunc A() int {\n\treturn 1\n}\n")
	write("ui/src/a.ts", "export const a = 1\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/master", "HEAD")
	write("lib/b.go", "package lib\n\nfunc B(x int) int {\n\tif x > 0 {\n\t\treturn x\n\t}\n\treturn 0\n}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "add B")
	write(".ship/go.out", profile)
	write("ui/coverage/lcov.info", lcov)
	c, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return dir, c
}

const lcovOK = "SF:src/a.ts\nDA:1,1\nend_of_record\n"

func TestGates_whenBranchCodeTested_thenPass(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 1\nexample.com/m/lib/b.go:3.20,4.11 1 1\n" +
		"example.com/m/lib/b.go:4.11,6.3 1 1\nexample.com/m/lib/b.go:7.2,7.10 1 1\n"
	dir, c := fixture(t, "ui: {}\n", prof, lcovOK)
	if err := (runner{root: dir, c: c}).gates(); err != nil {
		t.Fatal(err)
	}
}

func TestGates_whenBranchCodeUntested_thenPatchFails(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 9\nexample.com/m/lib/b.go:3.20,4.11 1 0\n" +
		"example.com/m/lib/b.go:4.11,6.3 1 0\nexample.com/m/lib/b.go:7.2,7.10 1 0\n"
	dir, c := fixture(t, "go:\n  floor: 10\n", prof, lcovOK)
	err := (runner{root: dir, c: c}).gates()
	if err == nil || !strings.Contains(err.Error(), "lib/b.go: missing lines") {
		t.Fatalf("err = %v", err)
	}
}

func TestGates_whenSkipMarker_thenOnlyFloors(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 9\nexample.com/m/lib/b.go:3.20,4.11 1 0\n"
	dir, c := fixture(t, "go:\n  floor: 10\n", prof, lcovOK)
	if _, err := gitx.Git(dir, "commit", "-q", "--allow-empty", "-m", "fmt "+SkipPatch); err != nil {
		t.Fatal(err)
	}
	if err := (runner{root: dir, c: c}).gates(); err != nil {
		t.Fatal(err)
	}
}

func TestGates_whenBelowFloor_thenFail(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 0\n"
	dir, c := fixture(t, "patch: 1\n", prof, lcovOK)
	if err := (runner{root: dir, c: c}).gates(); err == nil || !strings.Contains(err.Error(), "floor") {
		t.Fatalf("err = %v", err)
	}
}

func TestGates_whenReportsMissingOrEmpty_thenFail(t *testing.T) {
	dir, c := fixture(t, "ui: {}\n", "mode: atomic\nother.com/x/x.go:1.1,2.2 1 1\n", lcovOK)
	if err := (runner{root: dir, c: c}).gates(); err == nil || !strings.Contains(err.Error(), "no lines of module") {
		t.Fatalf("foreign-only profile: %v", err)
	}
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 1\n"
	dir, c = fixture(t, "ui: {}\n", prof, "")
	if err := (runner{root: dir, c: c}).gates(); err == nil || !strings.Contains(err.Error(), "holds no lines") {
		t.Fatalf("empty lcov: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, c.UI.Lcov)); err != nil {
		t.Fatal(err)
	}
	if err := (runner{root: dir, c: c}).gates(); err == nil || !strings.Contains(err.Error(), "ui coverage report") {
		t.Fatalf("missing lcov: %v", err)
	}
}

func TestGofmtAndSteps(t *testing.T) {
	dir, c := fixture(t, "steps:\n  - name: ok\n    run: test -f go.mod\n", "mode: set\n", "")
	r := runner{root: dir, c: c}
	if err := r.gofmt(); err != nil {
		t.Fatal(err)
	}
	if err := r.extra(); err != nil {
		t.Fatal(err)
	}
	if err := r.uiBuild(); err != nil {
		t.Fatal(err)
	}
	if err := r.uiTest(); err != nil {
		t.Fatal(err)
	}
	if err := r.uiLint(); err != nil {
		t.Fatal(err)
	}
	c.UI = &config.UI{Dir: "ui", Build: "test -d src", Lint: "exit 4", Test: "true"}
	if err := r.uiBuild(); err != nil {
		t.Fatal(err)
	}
	if err := r.uiLint(); err == nil {
		t.Fatal("ui lint failure ignored")
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "c.go"), []byte("package lib\nfunc  C() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.gofmt(); err == nil || !strings.Contains(err.Error(), "lib/c.go") {
		t.Fatalf("gofmt: %v", err)
	}
	c.Steps = []config.Step{{Name: "bad", Run: "exit 3"}}
	if err := r.extra(); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("step: %v", err)
	}
}

func TestModulePath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(p, []byte("// c\nmodule \"example.com/q\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := modulePath(p); err != nil || m != "example.com/q" {
		t.Fatalf("%s %v", m, err)
	}
	if err := os.WriteFile(p, []byte("go 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := modulePath(p); err == nil {
		t.Fatal("no module line accepted")
	}
}
