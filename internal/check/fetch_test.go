package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// withOrigin points the fixture's origin at a new bare repo, empty: a new repo's remote before its first push.
func withOrigin(t *testing.T, dir string) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	for _, args := range [][]string{{"init", "-q", "--bare", remote}, {"remote", "add", "origin", remote}} {
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return remote
}

func TestFetch_whenOriginHasNoBranch_thenNoBase(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	withOrigin(t, dir)
	noBase, err := (runner{root: dir, c: c}).fetch()
	if err != nil || !noBase {
		t.Fatalf("empty origin: noBase %v err %v", noBase, err)
	}
}

func TestFetch_whenBaseOnOrigin_thenFetched(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	withOrigin(t, dir)
	if _, err := gitx.Git(dir, "push", "-q", "--no-verify", "origin", "HEAD~1:refs/heads/master"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(dir, "update-ref", "-d", "refs/remotes/origin/master"); err != nil {
		t.Fatal(err)
	}
	noBase, err := (runner{root: dir, c: c}).fetch()
	if err != nil || noBase {
		t.Fatalf("noBase %v err %v", noBase, err)
	}
	if got, want := mustRev(t, dir, "origin/master"), mustRev(t, dir, "HEAD~1"); got != want {
		t.Fatalf("origin/master %s, want %s", got, want)
	}
}

// An origin git cannot reach is never no base: the fetch's failure stands.
func TestFetch_whenFetchFailsOtherwise_thenFails(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	r := runner{root: dir, c: c}
	if noBase, err := r.fetch(); err == nil || noBase {
		t.Fatalf("no origin: noBase %v err %v", noBase, err)
	}
	remote := withOrigin(t, dir)
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	if noBase, err := r.fetch(); err == nil || noBase {
		t.Fatalf("origin unreachable: noBase %v err %v", noBase, err)
	}
}

// An origin with branches but no <base> (a feature pushed first) is no base only for the push that creates the base:
// the hook's refs name it, or by hand HEAD is on it. A typo in base: names neither and still fails.
func TestFetch_whenOriginHasOtherBranchesOnly_thenOnlyTheBasePushPasses(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	withOrigin(t, dir)
	if _, err := gitx.Git(dir, "push", "-q", "--no-verify", "origin", "HEAD:refs/heads/feature"); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		base    string
		pushing []string
		noBase  bool
	}{
		"by hand on the base":     {"master", nil, true},
		"hook pushes the base":    {"master", []string{"refs/heads/feature", "refs/heads/master"}, true},
		"hook pushes another":     {"master", []string{"refs/heads/feature"}, false},
		"by hand, base: mistyped": {"mastr", nil, false},
		"hook, base: mistyped":    {"mastr", []string{"refs/heads/master"}, false},
	} {
		c.Base = tc.base
		noBase, err := (runner{root: dir, c: c, pushing: tc.pushing}).fetch()
		if tc.noBase {
			if err != nil || !noBase {
				t.Errorf("%s: noBase %v err %v", name, noBase, err)
			}
			continue
		}
		want := "origin has branches but no " + tc.base + ": push " + tc.base + " first"
		if err == nil || noBase || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "fix base:") {
			t.Errorf("%s: noBase %v err %v", name, noBase, err)
		}
	}
}

func mustRev(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := gitx.Git(dir, "rev-parse", rev)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// With no base, every commit of HEAD is the branch's: the root commit's subject too.
func TestSubjects_whenNoBase_thenEveryCommit(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	t.Setenv(PRTitle, "")
	if _, err := gitx.Git(dir, "commit", "-q", "--amend", "-m", "lib: add B"); err != nil {
		t.Fatal(err)
	}
	if err := (runner{root: dir, c: c}).subjects(); err != nil {
		t.Fatalf("with a base: %v", err)
	}
	err := (runner{root: dir, c: c, noBase: true}).subjects()
	if err == nil || !strings.Contains(err.Error(), "1 of 2") || !strings.Contains(err.Error(), " base: want") ||
		!strings.Contains(err.Error(), "git rebase -i --root") {
		t.Fatalf("root commit's subject not checked: %v", err)
	}
}

// A key the root commit adds and a later one removes reaches origin on the first push, log.showRoot off or not.
func TestSecrets_whenNoBaseAndKeyInRootCommit_thenFail(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "master")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	git("config", "commit.gpgsign", "false")
	git("config", "log.showRoot", "false")
	writeFile(t, dir, "k.go", "package k\n\nconst k = \""+key("sk-")+"\"\n")
	git("add", "-A")
	git("commit", "-q", "-m", "k: add")
	git("rm", "-q", "k.go")
	git("commit", "-q", "--allow-empty", "-m", "k: drop")
	c, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	err = (runner{root: dir, c: c, noBase: true}).secrets()
	if err == nil || !strings.Contains(err.Error(), "k.go: sk-Ab3…") || !strings.Contains(err.Error(), "--root") {
		t.Fatalf("root commit's key passed: %v", err)
	}
}

// With no base, the patch gate counts every line of the tree: the base commit's untested lib/a.go fails it.
func TestGates_whenNoBase_thenPatchOverWholeTree(t *testing.T) {
	prof := "mode: atomic\nexample.com/m/lib/a.go:3.14,5.2 1 0\nexample.com/m/lib/b.go:3.20,4.11 1 1\n" +
		"example.com/m/lib/b.go:4.11,6.3 1 1\nexample.com/m/lib/b.go:7.2,7.10 1 1\n"
	dir, c := fixture(t, "patch: 100\ngo:\n  floor: 10\n", prof, "")
	if err := (runner{root: dir, c: c}).gates(); err != nil {
		t.Fatalf("with a base: %v", err)
	}
	err := (runner{root: dir, c: c, noBase: true}).gates()
	if err == nil || !strings.Contains(err.Error(), "lib/a.go: missing lines") {
		t.Fatalf("no base: lib/a.go not counted: %v", err)
	}
}
