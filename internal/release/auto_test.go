package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

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

// shipTagged is autoClone's lib.go tagged v1.1.0 and pushed, for a change on top.
func shipTagged(t *testing.T, msg string) (work string, git func(...string)) {
	t.Helper()
	work = autoClone(t, "lib.go", msg)
	git = func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("tag", "v1.1.0")
	git("push", "-q", "--no-verify", "origin", "v1.1.0")
	return work, git
}

func TestRunAuto_whenShippedFileMovedIntoTestdata_thenReleased(t *testing.T) {
	work, git := shipTagged(t, "lib: add")
	if err := os.Mkdir(filepath.Join(work, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("mv", "lib.go", "testdata/lib.go")
	git("commit", "-q", "-m", "lib: move to testdata")
	git("push", "-q", "--no-verify", "origin", "master")
	out, err := runAuto(t, work)
	if err != nil || !strings.Contains(out, "1.1.0 -> 1.1.1 (auto: patch)") {
		t.Fatalf("a rename hid the removed shipped file: %v\n%s", err, out)
	}
}

func TestRunAuto_whenAheadOfOrigin_thenRefused(t *testing.T) {
	work, git := shipTagged(t, "lib: add")
	git("commit", "-q", "--allow-empty", "-m", "lib: local only")
	if out, err := runAuto(t, work); err == nil || !strings.Contains(err.Error(), "is not origin/master") {
		t.Fatalf("ahead skipped as behind: %v\n%s", err, out)
	}
}

func TestRun_whenBumpByHand_thenReleaseNotesKept(t *testing.T) {
	// A typo stops auto; the fallback by hand still carries the note.
	work := autoClone(t, "lib.go", "lib: drop Old\n\nRelease: minr\nRelease-note: Old is gone.")
	if _, err := runAuto(t, work); err == nil || !strings.Contains(err.Error(), "nothing tagged") {
		t.Fatalf("auto passed a typo: %v", err)
	}
	c, _ := config.Parse([]byte("tag_prefix: v\n"))
	out, err := capture(t, func() error { return Run(work, c, Options{Bump: "minor", DryRun: true}) })
	if err != nil || !strings.Contains(out, "(minor)") || !strings.Contains(out, "--notes Old is gone.\n\n**lib**") {
		t.Fatalf("%v\n%s", err, out)
	}
}
