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
// profile in the git dir (ship/go.out) and an lcov report, so the gates run without go test or npm.
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
	write(".gitignore", "ui/coverage/\n")
	write("lib/a.go", "package lib\n\nfunc A() int {\n\treturn 1\n}\n")
	write("ui/src/a.ts", "export const a = 1\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/master", "HEAD")
	write("lib/b.go", "package lib\n\nfunc B(x int) int {\n\tif x > 0 {\n\t\treturn x\n\t}\n\treturn 0\n}\n")
	git("add", "-A")
	git("commit", "-q", "-m", "add B")
	write(".git/ship/go.out", profile)
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

func TestGates_whenUIReportPathsOff_thenFailWithGoResultsToo(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 0\nexample.com/m/lib/b.go:3.20,4.11 1 1\n" +
		"example.com/m/lib/b.go:4.11,6.3 1 1\nexample.com/m/lib/b.go:7.2,7.10 1 1\n"
	// vitest rooted one level off: SF:a.ts lands on ui/a.ts, but the file is ui/src/a.ts. The Go floor (100) fails
	// too and must be reported in the same run.
	dir, c := fixture(t, "ui: {}\ngo:\n  floor: 100\n", prof, "SF:a.ts\nDA:1,1\nend_of_record\n")
	err := (runner{root: dir, c: c}).gates()
	if err == nil || !strings.Contains(err.Error(), "ui coverage report names files") || !strings.Contains(err.Error(), "ui/a.ts") {
		t.Fatalf("broken ui mapping passed: %v", err)
	}
	if !strings.Contains(err.Error(), "go coverage") {
		t.Fatalf("go floor result hidden by the ui error: %v", err)
	}
}

// A broken report skips the patch gate instead of computing it over the other side only: the branch's untested
// lib/b.go would otherwise fail it, and a pass over partial data would read as green.
func TestGates_whenUIReportMissing_thenPatchNotRun(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 9\nexample.com/m/lib/b.go:3.20,4.11 1 0\n" +
		"example.com/m/lib/b.go:4.11,6.3 1 0\nexample.com/m/lib/b.go:7.2,7.10 1 0\n"
	dir, c := fixture(t, "ui: {}\ngo:\n  floor: 10\n", prof, lcovOK)
	if err := os.Remove(filepath.Join(dir, c.UI.Lcov)); err != nil {
		t.Fatal(err)
	}
	err := (runner{root: dir, c: c}).gates()
	if err == nil || !strings.Contains(err.Error(), "ui coverage report") {
		t.Fatalf("missing ui report passed: %v", err)
	}
	if strings.Contains(err.Error(), "patch coverage") {
		t.Fatalf("patch gate ran over the Go side only: %v", err)
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

func TestIgnoredByGo(t *testing.T) {
	for p, want := range map[string]bool{
		"a.go":                     false,
		"internal/x/a.go":          false,
		"internal/testdata/bad.go": true,
		"vendor/m/a.go":            true,
		"_old/a.go":                true,
		".hidden/a.go":             true,
		"internal/_a.go":           true, // go build ignores _ and . files too
		"internal/.b.go":           true,
		"internal/testdata.go":     false,
	} {
		if got := ignoredByGo(p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestGofmt_whenBrokenFixtureInTestdata_thenIgnored(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	if err := os.MkdirAll(filepath.Join(dir, "lib", "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "testdata", "broken.go"), []byte("package (\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (runner{root: dir, c: c}).gofmt(); err != nil {
		t.Fatal(err)
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

func TestExtra_whenHookSetsGitDir_thenStepsDoNotSeeIt(t *testing.T) {
	dir, c := fixture(t, "steps:\n  - name: no git dir\n    run: test -z \"$GIT_DIR\"\n", "mode: set\n", "")
	t.Setenv("GIT_DIR", "/real/repo/.git/worktrees/x")
	t.Setenv("GIT_WORK_TREE", "/real/repo")
	if err := (runner{root: dir, c: c}).extra(); err != nil {
		t.Fatalf("GIT_DIR reached a step: %v", err)
	}
	if err := (runner{root: dir, c: c}).run(dir, nil, "sh", "-c", `test -z "$GIT_WORK_TREE"`); err != nil {
		t.Fatalf("GIT_WORK_TREE reached a command: %v", err)
	}
}

func TestSubjects(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	r := runner{root: dir, c: c}
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(PRTitle, "")
	// The fixture's branch commit "add B" has no area.
	err := r.subjects()
	if err == nil || !strings.Contains(err.Error(), "1 of 1") || !strings.Contains(err.Error(), "add B: want") {
		t.Fatalf("got %v", err)
	}
	git("commit", "-q", "--amend", "-m", "lib: add B")
	git("commit", "-q", "--allow-empty", "--author", "dependabot[bot] <bot@example.com>", "-m", "build(deps): bump x")
	git("commit", "-q", "--allow-empty", "-m", "fixup! lib: add B")
	if err := r.subjects(); err != nil {
		t.Fatalf("Go-style, bot and fixup subjects failed: %v", err)
	}
	t.Setenv(PRTitle, "Add B to lib")
	if err := r.subjects(); err == nil || !strings.Contains(err.Error(), "PR title Add B to lib") {
		t.Fatalf("PR title not checked: %v", err)
	}
	t.Setenv(PRTitle, "lib: add B")
	if err := r.subjects(); err != nil {
		t.Fatal(err)
	}
}
