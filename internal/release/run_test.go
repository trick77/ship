package release

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/pins"
)

// origin is a bare repo with master and tag 1.4.2 on master's parent; the clone's HEAD is origin/master, a main
// package that builds.
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
	writeMain(t, work, "package main\n\nfunc main() {}\n")
	git(work, "commit", "-q", "-m", "two")
	git(work, "remote", "add", "origin", origin)
	git(work, "push", "-q", "--no-verify", "origin", "master", "1.4.2")
	git(work, "fetch", "-q", "origin")
	return work
}

// writeMain writes and stages go.mod and main.go. Module mode forced: GOPATH mode builds anything.
func writeMain(t *testing.T, dir, src string) {
	t.Helper()
	t.Setenv("GO111MODULE", "on")
	for name, body := range map[string]string{"go.mod": "module ex\n\ngo 1.24\n", "main.go": src} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.Git(dir, "add", "go.mod", "main.go"); err != nil {
		t.Fatal(err)
	}
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
		"--notes **other**\n- two\n\nContainer images: `ghcr.io/owner/repo:1.5.0`",
		"--sbom=true --provenance=mode=max --metadata-file ",
		"--label org.opencontainers.image.version=1.5.0 --label org.opencontainers.image.revision=",
		"--label org.opencontainers.image.source=https://github.com/Owner/Repo --label org.opencontainers.image.created=",
		" ghcr.io/aquasecurity/trivy:" + pins.Trivy + " image --scanners vuln --no-progress --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed ghcr.io/owner/repo-dashboard@<digest>",
		"+ cosign sign --yes ghcr.io/owner/repo@<digest>",
		"+ cosign sign --yes ghcr.io/owner/repo-dashboard@<digest>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !regexp.MustCompile(`image\.revision=[0-9a-f]{40} .*image\.created=\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ `).MatchString(out) {
		t.Errorf("revision or created label malformed:\n%s", out)
	}
	tag, build, rel := strings.Index(out, "gh api"), strings.Index(out, "docker buildx build"), strings.Index(out, "gh release")
	scan, sign := strings.Index(out, "+ docker run --rm"), strings.Index(out, "+ cosign sign")
	retag := strings.Index(out, "docker buildx imagetools create -t ghcr.io/owner/repo:1.5 -t ghcr.io/owner/repo:1 -t ghcr.io/owner/repo:latest ghcr.io/owner/repo:1.5.0")
	if tag < 0 || retag < 0 || tag > build || build > scan || scan > sign || sign > rel || rel > retag {
		t.Errorf("want tag claim < build < scan < sign < release < moving tags, got %d %d %d %d %d %d", tag, build, scan, sign, rel, retag)
	}
	if strings.Contains(out[:rel], ":latest") {
		t.Error(":latest pushed before the release exists")
	}
	if strings.Contains(out, "go build") {
		t.Error("go build with images: the image build compiles")
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
	if strings.Contains(out, "docker") || !strings.Contains(out, "ref=refs/tags/v0.0.1") || !strings.Contains(out, "released v0.0.1") ||
		!strings.Contains(out, "--notes Release 0.0.1") {
		t.Fatalf("plain 1.4.2 must not count for prefix v:\n%s", out)
	}
	if build, tag := strings.Index(out, "+ go build -o "+os.DevNull+" ./..."), strings.Index(out, "+ gh api"); build < 0 || build > tag {
		t.Fatalf("want go build before the tag claim:\n%s", out)
	}
	// A main package: without -o the build leaves a binary ex in the tree.
	if dirty, err := gitx.Dirty(work); err != nil || dirty != "" {
		t.Fatalf("tree after the build: %q %v", dirty, err)
	}
}

func TestRun_whenUINoImages_thenNoBuild(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	c, _ := config.Parse([]byte("ui: {}\n"))
	out, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch", DryRun: true}) })
	if err != nil || strings.Contains(out, "go build") {
		t.Fatalf("go build with a UI (its embed needs node): %v\n%s", err, out)
	}
}

func TestRun_whenNoImagesBuildFails_thenNothingTagged(t *testing.T) {
	work := clone(t)
	writeMain(t, work, "package main\n\nfunc main() { broken }\n")
	for _, args := range [][]string{{"commit", "-q", "-m", "broken"}, {"push", "-q", "--no-verify", "origin", "master"}} {
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	log := fakeTools(t, "")
	c, _ := config.Parse([]byte("tag_prefix: v\n"))
	_, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch"}) })
	if err == nil || !strings.Contains(err.Error(), "go build (nothing tagged)") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(log); len(b) > 0 {
		t.Fatalf("gh or docker ran after a failed build:\n%s", b)
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

// fakeTools puts docker, gh and cosign scripts first on PATH: each logs its arguments; docker buildx writes a digest
// to its --metadata-file. A call whose "<tool> <args>" starts with failOn exits 1. It poses as a GitHub-hosted
// runner: images build nowhere else.
func fakeTools(t *testing.T, failOn string) (log string) {
	t.Helper()
	t.Setenv("RUNNER_ENVIRONMENT", "github-hosted")
	bin := t.TempDir()
	log = filepath.Join(bin, "calls.log")
	for _, name := range []string{"docker", "gh", "cosign"} {
		script := fmt.Sprintf("#!/bin/sh\necho %s \"$@\" >> %s\n", name, log) +
			"prev=\nfor a in \"$@\"; do\n" +
			"  if [ \"$prev\" = --metadata-file ]; then echo '{\"containerimage.digest\":\"sha256:feed\"}' > \"$a\"; fi\n" +
			"  prev=$a\ndone\n"
		if failOn != "" {
			script += fmt.Sprintf("case \"%s $*\" in %q*) exit 1 ;; esac\n", name, failOn)
		}
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script+"exit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestRun_whenBuildScanOrSignFails_thenClaimedTagDeleted(t *testing.T) {
	for failOn, want := range map[string]string{
		"docker buildx build": "image ghcr.io/o/r",
		"docker run":          "trivy ghcr.io/o/r@sha256:feed",
		"cosign sign":         "cosign sign ghcr.io/o/r@sha256:feed",
	} {
		t.Run(failOn, func(t *testing.T) {
			work := clone(t)
			t.Setenv("GITHUB_REPOSITORY", "o/r")
			log := fakeTools(t, failOn)
			c, _ := config.Parse([]byte("images:\n  - {}\n"))
			_, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch"}) })
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v", err)
			}
			b, _ := os.ReadFile(log)
			calls := string(b)
			claim, del := strings.Index(calls, "gh api repos/o/r/git/refs -f ref=refs/tags/1.4.3"), strings.Index(calls, "gh api -X DELETE repos/o/r/git/refs/tags/1.4.3")
			if claim < 0 || del < claim || strings.Contains(calls, "release create") || strings.Contains(calls, "imagetools") {
				t.Fatalf("want claim, failure, delete, no release:\n%s", calls)
			}
			if failOn != "cosign sign" && strings.Contains(calls, "cosign") {
				t.Fatalf("signed after a failed build or scan:\n%s", calls)
			}
		})
	}
}

func TestRun_whenAllSucceeds_thenScannedSignedNoDelete(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	log := fakeTools(t, "")
	c, _ := config.Parse([]byte("images:\n  - {}\n  - suffix: -x\n"))
	if _, err := capture(t, func() error { return Run(work, c, Options{Bump: "patch"}) }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	if strings.Contains(calls, "DELETE") || !strings.Contains(calls, "imagetools create") {
		t.Fatalf("calls:\n%s", calls)
	}
	// Both scans before the first signature, the signatures before the release.
	scan2, sign1 := strings.Index(calls, "image --scanners vuln --no-progress --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed ghcr.io/o/r-x@sha256:feed"),
		strings.Index(calls, "cosign sign --yes ghcr.io/o/r@sha256:feed")
	sign2, rel := strings.Index(calls, "cosign sign --yes ghcr.io/o/r-x@sha256:feed"), strings.Index(calls, "gh release create")
	if scan2 < 0 || sign1 < scan2 || sign2 < sign1 || rel < sign2 {
		t.Fatalf("want scans < signatures < release:\n%s", calls)
	}
}

func TestDigestRef_whenMetadataBroken_thenError(t *testing.T) {
	dir := t.TempDir()
	if _, err := digestRef("i", filepath.Join(dir, "none.json"), false); err == nil {
		t.Error("missing metadata file accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	for body, want := range map[string]string{`{"containerimage.digest":""}`: "no digest", `{`: "buildx metadata"} {
		if err := os.WriteFile(bad, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := digestRef("i", bad, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s accepted: %v", body, err)
		}
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
