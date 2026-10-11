package release

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// origin is a bare repo with master and tag 1.4.2; the clone's HEAD is origin/master.
func clone(t *testing.T) string {
	t.Helper()
	origin, work := t.TempDir(), t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	git(origin, "init", "-q", "--bare", "-b", "master")
	git(work, "init", "-q", "-b", "master")
	git(work, "config", "user.email", "t@example.com")
	git(work, "config", "user.name", "t")
	git(work, "config", "commit.gpgsign", "false")
	git(work, "commit", "-q", "--allow-empty", "-m", "one")
	git(work, "tag", "1.4.2")
	git(work, "remote", "add", "origin", origin)
	git(work, "push", "-q", "--no-verify", "origin", "master", "1.4.2")
	git(work, "fetch", "-q", "origin")
	return work
}

func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = old
	_ = w.Close()
	b, _ := io.ReadAll(r)
	return string(b), runErr
}

func TestRun_whenDryRun_thenImagesThenTagThenRelease(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "Owner/Repo")
	c, err := config.Parse([]byte("images:\n  - {}\n  - suffix: -dashboard\n    target: dashboard\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := capture(t, func() error { return Run(work, c, Options{Bump: "minor", Cache: "gha", DryRun: true}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1.4.2 -> 1.5.0 (minor)",
		"-t ghcr.io/owner/repo:1.5.0",
		"--target dashboard",
		"-t ghcr.io/owner/repo-dashboard:latest",
		"--cache-from type=gha --cache-to type=gha,mode=max",
		"--build-arg VERSION=1.5.0",
		"gh api repos/Owner/Repo/git/refs -f ref=refs/tags/1.5.0",
		"gh release create 1.5.0 --repo Owner/Repo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "docker buildx") > strings.Index(out, "gh api") {
		t.Error("tag before image push")
	}
}

func TestRun_whenTagPrefixNoImages_thenPrefixedTagOnly(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	c, _ := config.Parse([]byte("tag_prefix: v\n"))
	out, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch", DryRun: true}) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "docker") || !strings.Contains(out, "ref=refs/tags/v0.0.1") || !strings.Contains(out, "released v0.0.1") {
		t.Fatalf("plain 1.4.2 must not count for prefix v:\n%s", out)
	}
}

func TestRun_whenNotOnOriginOrDirty_thenRefused(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	c, _ := config.Parse(nil)
	if err := os.WriteFile(work+"/wip", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch", DryRun: true}) }); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty: %v", err)
	}
	if _, err := gitx.Git(work, "add", "wip"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(work, "commit", "-q", "-m", "local"); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch", DryRun: true}) }); err == nil || !strings.Contains(err.Error(), "not origin/master") {
		t.Fatalf("ahead: %v", err)
	}
}

func TestRun_whenBadBump_thenError(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	c, _ := config.Parse(nil)
	if _, err := capture(t, func() error { return Run(work, c, Options{Bump: "x", DryRun: true}) }); err == nil {
		t.Fatal("bad bump accepted")
	}
}
