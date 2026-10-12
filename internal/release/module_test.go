package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// kitClone is clone with the root tagged v1.0.0 and two nested modules on top, kit/ and kit/auth/ (module ex/kit,
// ex/kit/auth), pushed untagged. commit writes, commits and pushes files.
func kitClone(t *testing.T) (work string, commit func(msg string, files ...string)) {
	t.Helper()
	work = clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
	commit = func(msg string, files ...string) {
		t.Helper()
		for _, f := range files {
			// An existing file gets a comment line more, a new one its package or module line.
			body := "package " + filepath.Base(filepath.Dir(f)) + "\n"
			if b, err := os.ReadFile(filepath.Join(work, f)); err == nil {
				body = string(b) + "// " + strings.SplitN(msg, "\n", 2)[0] + "\n"
			} else if filepath.Base(f) == "go.mod" {
				body = "module ex/" + filepath.Dir(f) + "\n\ngo 1.24\n"
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(work, f)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, f), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			git("add", f)
		}
		git("commit", "-q", "--allow-empty", "-m", msg)
		git("push", "-q", "--no-verify", "origin", "master")
	}
	git("tag", "v1.0.0")
	git("push", "-q", "--no-verify", "origin", "v1.0.0")
	commit("kit: add", "kit/go.mod", "kit/kit.go", "kit/auth/go.mod", "kit/auth/auth.go")
	return work, commit
}

func tagPush(t *testing.T, work, tag string) {
	t.Helper()
	for _, args := range [][]string{{"tag", tag}, {"push", "-q", "--no-verify", "origin", tag}} {
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func runDir(t *testing.T, work, dir, bump string, yaml ...string) (string, error) {
	t.Helper()
	c, err := config.Parse([]byte("tag_prefix: v\n" + strings.Join(yaml, "")))
	if err != nil {
		t.Fatal(err)
	}
	return capture(t, func() error { return Run(work, c, Options{Bump: bump, DryRun: true, Dir: dir}) })
}

func TestRunDir_whenNoTagYet_thenAutoCutsV010(t *testing.T) {
	work, _ := kitClone(t)
	// images: are the repo's: a nested module never builds them, nor needs a GitHub-hosted runner.
	t.Setenv("RUNNER_ENVIRONMENT", "")
	out, err := runDir(t, work, "kit/", Auto, "images:\n  - {}\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"0.0.0 -> 0.1.0 (auto: minor)", "ref=refs/tags/kit/v0.1.0",
		"gh release create kit/v0.1.0 --repo o/r --title kit/v0.1.0 --notes Release 0.1.0 --latest=false",
		"+ go build -o " + os.DevNull + " ./...", "released kit/v0.1.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "docker") {
		t.Errorf("images built for a nested module:\n%s", out)
	}
	// By hand, the same first tag.
	if out, err := runDir(t, work, "kit/auth", "minor"); err != nil || !strings.Contains(out, "ref=refs/tags/kit/auth/v0.1.0") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunDir_whenOnlyNestedChanged_thenScoped(t *testing.T) {
	work, commit := kitClone(t)
	tagPush(t, work, "v1.0.1")
	tagPush(t, work, "kit/v0.1.0")
	tagPush(t, work, "kit/auth/v0.1.0")
	commit("auth: fix\n\nRelease: minor\nRelease-note: auth changed.", "kit/auth/auth.go")
	commit("root: fix", "main.go")
	// kit ignores kit/auth/, the root ignores both; kit/auth sees its own commit and Release lines only.
	if out, err := runDir(t, work, "kit", Auto); err != nil || !strings.Contains(out, "SKIPPED: only docs") {
		t.Fatalf("kit: %v\n%s", err, out)
	}
	out, err := runDir(t, work, "kit/auth", Auto)
	if err != nil || !strings.Contains(out, "0.1.0 -> 0.2.0 (auto: minor)") ||
		!strings.Contains(out, "--notes auth changed.\n\n**auth**\n- fix --latest=false") {
		t.Fatalf("kit/auth: %v\n%s", err, out)
	}
	out, err = runDir(t, work, "", Auto)
	if err != nil || !strings.Contains(out, "1.0.1 -> 1.0.2 (auto: patch)") || !strings.Contains(out, "--notes **root**\n- fix\n") ||
		strings.Contains(out, "latest=false") {
		t.Fatalf("root: %v\n%s", err, out)
	}
}

func TestRunDir_whenRootHasImages_thenNestedChangesCount(t *testing.T) {
	work, commit := kitClone(t)
	tagPush(t, work, "v1.0.1")
	commit("auth: fix", "kit/auth/auth.go")
	t.Setenv("RUNNER_ENVIRONMENT", "")
	// A build context may copy kit/: the image's release is not cut down to the root module.
	if out, err := runDir(t, work, "", Auto, "images:\n  - {}\n"); err != nil || !strings.Contains(out, "1.0.1 -> 1.0.2") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRunDir_whenNoModule_thenSkippedOrRefused(t *testing.T) {
	work, _ := kitClone(t)
	if out, err := runDir(t, work, "lib", Auto); err != nil || !strings.Contains(out, "SKIPPED: no module at lib") {
		t.Fatalf("auto: %v\n%s", err, out)
	}
	if _, err := runDir(t, work, "lib", "patch"); err == nil || !strings.Contains(err.Error(), "no go.mod") {
		t.Fatalf("by hand: %v", err)
	}
}

func TestRunDir_whenBuildFails_thenNothingTagged(t *testing.T) {
	work, _ := kitClone(t)
	if err := os.WriteFile(filepath.Join(work, "kit/broken.go"), []byte("package kit\n\nfunc f() { broken }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "kit/broken.go"}, {"commit", "-q", "-m", "kit: break"}, {"push", "-q", "--no-verify", "origin", "master"}} {
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := runDir(t, work, "kit", "patch"); err == nil || !strings.Contains(err.Error(), "go build (nothing tagged)") ||
		strings.Contains(out, "gh api") {
		t.Fatalf("%v\n%s", err, out)
	}
	// The root still builds: the failure was kit's.
	if _, err := runDir(t, work, "", "patch"); err != nil {
		t.Fatal(err)
	}
}

func TestNested_whenBadDir_thenError(t *testing.T) {
	v, _ := config.Parse([]byte("tag_prefix: v\n"))
	for _, d := range []string{"../x", "/abs", ".."} {
		if _, _, err := nested(v, d); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
	if c, d, err := nested(v, "./"); err != nil || d != "" || c != v {
		t.Errorf("./ is the repo: %v %q", err, d)
	}
	plain, _ := config.Parse(nil)
	if _, _, err := nested(plain, "kit"); err == nil || !strings.Contains(err.Error(), "tag_prefix: v") {
		t.Errorf("tag_prefix \"\": %v", err)
	}
}
