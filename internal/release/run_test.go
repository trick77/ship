package release

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// origin is a bare repo with master and tag 1.4.2 on master's parent; the clone's HEAD is origin/master.
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
	git(work, "commit", "-q", "--allow-empty", "-m", "two")
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

func TestRun_whenDryRun_thenTagImagesReleaseMovingTags(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "Owner/Repo")
	// Off a GitHub-hosted runner: a dry run with images is allowed anywhere.
	t.Setenv("RUNNER_ENVIRONMENT", "")
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
		"imagetools create -t ghcr.io/owner/repo-dashboard:1.5 -t ghcr.io/owner/repo-dashboard:1 -t ghcr.io/owner/repo-dashboard:latest ghcr.io/owner/repo-dashboard:1.5.0",
		"--cache-from type=gha,scope=ghcr.io/owner/repo --cache-to type=gha,scope=ghcr.io/owner/repo,mode=max",
		"--cache-to type=gha,scope=ghcr.io/owner/repo-dashboard,mode=max",
		"--build-arg VERSION=1.5.0",
		"gh api repos/Owner/Repo/git/refs -f ref=refs/tags/1.5.0",
		"gh release create 1.5.0 --repo Owner/Repo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	tag, build, rel := strings.Index(out, "gh api"), strings.Index(out, "docker buildx build"), strings.Index(out, "gh release")
	retag := strings.Index(out, "docker buildx imagetools create -t ghcr.io/owner/repo:1.5 -t ghcr.io/owner/repo:1 -t ghcr.io/owner/repo:latest ghcr.io/owner/repo:1.5.0")
	if tag < 0 || retag < 0 || tag > build || build > rel || rel > retag {
		t.Errorf("want tag claim < build < release < moving tags, got %d %d %d %d", tag, build, rel, retag)
	}
	if strings.Contains(out[:rel], ":latest") {
		t.Error(":latest pushed before the release exists")
	}
}

func TestRun_whenImagesOffGitHub_thenRefusedBeforeAnyCommand(t *testing.T) {
	c, err := config.Parse([]byte("images:\n  - {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Images) != 1 {
		t.Fatalf("images: %+v", c.Images)
	}
	// Self-hosted runner, then a laptop (no RUNNER_ENVIRONMENT at all).
	for _, env := range []string{"self-hosted", ""} {
		t.Setenv("RUNNER_ENVIRONMENT", env)
		if env == "" {
			_ = os.Unsetenv("RUNNER_ENVIRONMENT")
		}
		// No clone: the refusal comes before git, gh or docker run.
		out, err := capture(t, func() error { return Run(t.TempDir(), c, Options{Bump: "patch"}) })
		if err == nil || !strings.Contains(err.Error(), "GitHub-hosted runners only") || out != "" {
			t.Fatalf("RUNNER_ENVIRONMENT %q: err %v, out %q", env, err, out)
		}
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

func TestRun_whenHeadAlreadyTagged_thenRefusedWithRecovery(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	if _, err := gitx.Git(work, "tag", "1.4.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(work, "push", "-q", "--no-verify", "origin", "1.4.3"); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Parse(nil)
	_, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch", DryRun: true}) })
	if err == nil || !strings.Contains(err.Error(), "already tagged 1.4.3") || !strings.Contains(err.Error(), "gh release create 1.4.3") ||
		!strings.Contains(err.Error(), "git/refs/tags/1.4.3") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_whenLocalOnlyTag_thenIgnored(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	for _, tag := range []string{"9.9.9", "1.4.3"} { // 1.4.3 on HEAD, local only
		if _, err := gitx.Git(work, "tag", tag); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := config.Parse(nil)
	out, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch", DryRun: true}) })
	if err != nil || !strings.Contains(out, "1.4.2 -> 1.4.3") {
		t.Fatalf("local tags counted: %v\n%s", err, out)
	}
}

// fakeTools puts docker and gh scripts first on PATH: each logs its arguments, docker exits dockerRC. It poses as a
// GitHub-hosted runner: images build nowhere else.
func fakeTools(t *testing.T, dockerRC int) (log string) {
	t.Helper()
	t.Setenv("RUNNER_ENVIRONMENT", "github-hosted")
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	for name, rc := range map[string]int{"docker": dockerRC, "gh": 0} {
		script := fmt.Sprintf("#!/bin/sh\necho %s \"$@\" >> %s\nexit %d\n", name, log, rc)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestRun_whenBuildFails_thenClaimedTagDeleted(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	log := fakeTools(t, 1)
	c, _ := config.Parse([]byte("images:\n  - {}\n"))
	_, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch"}) })
	if err == nil || !strings.Contains(err.Error(), "image ghcr.io/o/r") {
		t.Fatalf("err = %v", err)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	claim, del := strings.Index(calls, "gh api repos/o/r/git/refs -f ref=refs/tags/1.4.3"), strings.Index(calls, "gh api -X DELETE repos/o/r/git/refs/tags/1.4.3")
	if claim < 0 || del < claim || strings.Contains(calls, "release create") {
		t.Fatalf("want claim, failed build, delete, no release:\n%s", calls)
	}
}

func TestRun_whenAllSucceeds_thenNoDelete(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	log := fakeTools(t, 0)
	c, _ := config.Parse([]byte("images:\n  - {}\n"))
	if _, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch"}) }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if calls := string(b); strings.Contains(calls, "DELETE") || !strings.Contains(calls, "imagetools create") {
		t.Fatalf("calls:\n%s", calls)
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
