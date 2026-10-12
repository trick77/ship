package release

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

func TestPlanOf(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []string
		bump     string
		notes    []string
		err      string
	}{
		{"none", []string{"hook: x\n\nbody\n", "gate: y\n"}, "patch", nil, ""},
		{"minor", []string{"hook: x\n\nRelease: minor\nRelease-note: hooks moved\n"}, "minor", []string{"hooks moved"}, ""},
		{"highest wins, notes in order", []string{
			"a: x\n\nRelease: major\nRelease-note: one\n",
			"b: y\n\nRelease: minor\nRelease-note: two\n",
		}, "major", []string{"one", "two"}, ""},
		{"note alone keeps patch", []string{"a: x\n\nRelease-note: fyi\n"}, "patch", []string{"fyi"}, ""},
		{"inside a line is no trailer", []string{"a: x\n\nsee Release: minor in docs\n"}, "patch", nil, ""},
		{"minor without note", []string{"a: x\n\nRelease: minor\n"}, "", nil, "without a `Release-note"},
		{"typo", []string{"a: x\n\nRelease: minr\n"}, "", nil, "want minor or major"},
		{"patch spelled out", []string{"a: x\n\nRelease: patch\n"}, "", nil, "patch is the default"},
	} {
		p, err := PlanOf(tc.messages)
		switch {
		case tc.err != "":
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: got %v, want error with %q", tc.name, err, tc.err)
			}
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case p.Bump != tc.bump || !slices.Equal(p.Notes, tc.notes):
			t.Errorf("%s: got %+v, want %s %v", tc.name, p, tc.bump, tc.notes)
		}
	}
}

func TestShipped(t *testing.T) {
	for p, want := range map[string]bool{
		"README.md": false, "docs/x.md": false, "LICENSE": false, "a_test.go": false, "internal/x/x_test.go": false,
		"testdata/ui/package.json": false, "internal/x/testdata/a": false,
		"main.go": true, "go.mod": true, ".github/workflows/check.yaml": true, ".golangci.yaml": true,
		"testdata.go": true,
	} {
		if Shipped(p) != want {
			t.Errorf("Shipped(%q) = %v", p, !want)
		}
	}
}

// autoClone is clone with ship's v-tags: v1.0.0 on origin/master, then commit adds msg on top (with file changed
// to body), pushed.
func autoClone(t *testing.T, file, msg string) string {
	t.Helper()
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("tag", "v1.0.0")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, file), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", file)
	git("commit", "-q", "-m", msg)
	git("push", "-q", "--no-verify", "origin", "master", "v1.0.0")
	return work
}

func runAuto(t *testing.T, work string) (string, error) {
	t.Helper()
	c, _ := config.Parse([]byte("tag_prefix: v\n"))
	return capture(t, func() error { return Run(work, c, Options{Bump: Auto, DryRun: true}) })
}

func TestRunAuto_whenDocsOnly_thenSkipped(t *testing.T) {
	out, err := runAuto(t, autoClone(t, "docs/a.md", "docs: explain"))
	if err != nil || !strings.Contains(out, "SKIPPED: only docs") || strings.Contains(out, "gh api") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunAuto_whenCode_thenPatch(t *testing.T) {
	out, err := runAuto(t, autoClone(t, "lib.go", "lib: add"))
	if err != nil || !strings.Contains(out, "1.0.0 -> 1.0.1 (auto: patch)") || !strings.Contains(out, "--notes **lib**\n- add") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunAuto_whenMinorTrailer_thenMinorWithNoteFirst(t *testing.T) {
	work := autoClone(t, "lib.go", "lib: drop Old\n\nRelease: minor\nRelease-note: Old is gone, use New.")
	out, err := runAuto(t, work)
	if err != nil || !strings.Contains(out, "(auto: minor)") ||
		!strings.Contains(out, "--notes Old is gone, use New.\n\n**lib**\n- drop Old") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunAuto_whenMinorWithoutNote_thenFailsUntagged(t *testing.T) {
	out, err := runAuto(t, autoClone(t, "lib.go", "lib: drop Old\n\nRelease: minor"))
	if err == nil || strings.Contains(out, "gh api") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunAuto_whenBehindOrigin_thenSkipped(t *testing.T) {
	work := autoClone(t, "lib.go", "lib: add")
	if _, err := gitx.Git(work, "reset", "-q", "--hard", "HEAD~1"); err != nil {
		t.Fatal(err)
	}
	out, err := runAuto(t, work)
	if err != nil || !strings.Contains(out, "SKIPPED: HEAD") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunAuto_whenNoPreviousTag_thenRefused(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	if _, err := runAuto(t, work); err == nil || !strings.Contains(err.Error(), "needs a previous tag") {
		t.Fatalf("got %v", err)
	}
}
