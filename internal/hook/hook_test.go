package hook

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/gitx"
)

const (
	zero    = "0000000000000000000000000000000000000000"
	zero256 = "0000000000000000000000000000000000000000000000000000000000000000"
)

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "master"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
		{"commit", "-q", "--allow-empty", "-m", "one"},
		{"commit", "-q", "--allow-empty", "-m", "two"},
	} {
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// shipOnGitHub points the hook's git ls-remote of ship on GitHub at src instead, via a global config of its own (the
// developer's must not reach the pushes either), so no test asks GitHub.
func shipOnGitHub(t *testing.T, src string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if _, err := gitx.Git("", "config", "--file", cfg, "url."+src+".insteadOf", "https://github.com/trick77/ship"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func TestParseRefsAndGated(t *testing.T) {
	in := "refs/heads/a 1111111111111111111111111111111111111111 refs/heads/a " + zero + "\n" +
		"(delete) " + zero + " refs/heads/old 2222222222222222222222222222222222222222\n" +
		"refs/tags/1.0.0 3333333333333333333333333333333333333333 refs/tags/1.0.0 " + zero + "\n\n"
	refs, err := ParseRefs(strings.NewReader(in))
	if err != nil || len(refs) != 3 {
		t.Fatalf("%v %v", refs, err)
	}
	g := Gated(refs)
	if len(g) != 1 || g[0].Local != "refs/heads/a" {
		t.Fatalf("%v", g)
	}
	if g := Gated([]Ref{{Local: "(delete)", LocalSHA: zero256}}); len(g) != 0 {
		t.Fatalf("SHA-256 delete gated: %v", g)
	}
	// The updated remote ref decides: a tag pushed onto a branch is gated, HEAD pushed as a tag is not.
	g = Gated([]Ref{
		{Local: "refs/tags/v1", LocalSHA: "1111111111111111111111111111111111111111", Remote: "refs/heads/master"},
		{Local: "HEAD", LocalSHA: "2222222222222222222222222222222222222222", Remote: "refs/tags/x"},
	})
	if len(g) != 1 || g[0].Remote != "refs/heads/master" {
		t.Fatalf("%v", g)
	}
	if _, err := ParseRefs(strings.NewReader("only two\n")); err == nil {
		t.Fatal("malformed accepted")
	}
}

func TestReady(t *testing.T) {
	dir := repo(t)
	head, _ := gitx.Head(dir)
	prev, _ := gitx.Git(dir, "rev-parse", "HEAD~1")
	if err := Ready(dir, []Ref{{Local: "refs/heads/master", LocalSHA: head}}); err != nil {
		t.Fatal(err)
	}
	if err := Ready(dir, []Ref{{Local: "refs/heads/old", LocalSHA: prev}}); err == nil || !strings.Contains(err.Error(), "check out what you push") {
		t.Fatalf("non-HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wip.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ready(dir, []Ref{{Local: "refs/heads/master", LocalSHA: head}}); err == nil || !strings.Contains(err.Error(), "wip.txt") {
		t.Fatalf("dirty: %v", err)
	}
	if err := Ready(dir, []Ref{{Local: "refs/heads/x", LocalSHA: "nope"}}); err == nil {
		t.Fatal("bad sha accepted")
	}
}

func TestInstall(t *testing.T) {
	dir := repo(t)
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "Installed by ship hooks") {
		t.Fatalf("%s %v", b, err)
	}
	if _, err := Install(dir); err != nil {
		t.Fatalf("reinstall over ours: %v", err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir); err == nil || !strings.Contains(err.Error(), "did not install") {
		t.Fatalf("foreign hook overwritten: %v", err)
	}
}

// Only ship's own hook with another script is stale: none, the current one or a foreign hook is not.
func TestStale(t *testing.T) {
	if Stale(t.TempDir()) {
		t.Fatal("no repo reported stale")
	}
	dir := repo(t)
	if Stale(dir) {
		t.Fatal("no hook reported stale")
	}
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if Stale(dir) {
		t.Fatal("fresh hook reported stale")
	}
	for s, want := range map[string]bool{
		"#!/bin/sh\n# " + marker + "\ngo run github.com/trick77/ship@latest pre-push\n": true,
		"#!/bin/sh\necho mine\n": false,
	} {
		if err := os.WriteFile(p, []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := Stale(dir); got != want {
			t.Errorf("%q: stale = %v, want %v", s, got, want)
		}
	}
}

// A stale hook of ship's is rewritten, by rename (a running sh keeps the old file); a current or foreign one is not.
func TestRefresh(t *testing.T) {
	t.Setenv("SHIP_CMD", "")
	dir := repo(t)
	if _, ok, err := Refresh(dir); ok || err != nil {
		t.Fatalf("no hook: %v %v", ok, err)
	}
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Refresh(dir); ok || err != nil {
		t.Fatalf("current hook: %v %v", ok, err)
	}
	old := "#!/bin/sh\n# " + marker + "\ngo run github.com/trick77/ship@latest pre-push\n"
	if err := os.WriteFile(p, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	running, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = running.Close() }()
	got, ok, err := Refresh(dir)
	if !ok || err != nil || got != p || Stale(dir) {
		t.Fatalf("stale hook: %q %v %v", got, ok, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode: %v %v", fi, err)
	}
	if b, err := io.ReadAll(running); err != nil || string(b) != old {
		t.Fatalf("running hook's file changed under it: %q %v", b, err)
	}
	if b, err := os.ReadFile(p + Backup); err != nil || string(b) != old {
		t.Fatalf("previous hook not kept: %q %v", b, err)
	}
	// SHIP_CMD's ship is not master's: a stale hook stays, with an error to warn about.
	if err := os.WriteFile(p, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIP_CMD", "go run .")
	if _, ok, err := Refresh(dir); ok || err == nil || !strings.Contains(err.Error(), "SHIP_CMD") {
		t.Fatalf("SHIP_CMD set: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(p); string(b) != old {
		t.Fatalf("hook refreshed under SHIP_CMD: %q", b)
	}
	t.Setenv("SHIP_CMD", "")
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".pre-push-*")); len(left) != 0 {
		t.Fatalf("leftovers: %v", left)
	}
	foreign := "#!/bin/sh\necho mine\n"
	if err := os.WriteFile(p, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Refresh(dir); ok || err != nil {
		t.Fatalf("foreign hook: %v %v", ok, err)
	}
	if b, _ := os.ReadFile(p); string(b) != foreign {
		t.Fatalf("foreign hook touched: %q", b)
	}
}

// A symlinked hook, even to ship's own script, is left alone: replacing it would cut the link.
func TestRefresh_whenHookSymlinked_thenLeft(t *testing.T) {
	t.Setenv("SHIP_CMD", "")
	dir := repo(t)
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "pre-push")
	old := "#!/bin/sh\n# " + marker + "\ngo run github.com/trick77/ship@latest pre-push\n"
	if err := os.WriteFile(target, []byte(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Refresh(dir); ok || err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked stale hook: %v %v", ok, err)
	}
	if _, err := Install(dir); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Install over a symlink: %v", err)
	}
	if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link replaced: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != old {
		t.Fatalf("target changed: %q", b)
	}
	if _, err := os.Stat(p + Backup); err == nil {
		t.Fatal("backup written for a hook left alone")
	}
}

func TestInstall_whenHooksPathInsideRepo_thenThere(t *testing.T) {
	dir := repo(t)
	if _, err := gitx.Git(dir, "config", "core.hooksPath", ".git/myhooks"); err != nil {
		t.Fatal(err)
	}
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(p)) != "myhooks" {
		t.Fatalf("installed at %s", p)
	}
}

func TestInstall_whenHooksPathInWorkingTree_thenRefusedAsTracked(t *testing.T) {
	dir := repo(t)
	if _, err := gitx.Git(dir, "config", "core.hooksPath", ".husky/_"); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir); err == nil || !strings.Contains(err.Error(), "in the working tree") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstall_whenHooksPathOutsideRepo_thenRefused(t *testing.T) {
	dir := repo(t)
	shared := filepath.Join(t.TempDir(), "hooks")
	if _, err := gitx.Git(dir, "config", "core.hooksPath", shared); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(dir); err == nil || !strings.Contains(err.Error(), "outside this repo") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(shared, "pre-push")); err == nil {
		t.Fatal("hook written to the shared dir")
	}
}

// The installed script: deletes and tags pass, a branch without ship.yaml is refused, one with it runs ship.
func TestScript(t *testing.T) {
	dir := repo(t)
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Head(dir)
	for in, wantOK := range map[string]bool{
		"(delete) " + zero + " refs/heads/x " + head + "\n":           true,
		"(delete) " + zero256 + " refs/heads/x " + head + "\n":        true,
		"refs/tags/1.0.0 " + head + " refs/tags/1.0.0 " + zero + "\n": true,
		"HEAD " + head + " refs/tags/x " + zero + "\n":                true,
		"refs/tags/v1 " + head + " refs/heads/master " + zero + "\n":  false,
		"": true,
		"refs/heads/master " + head + " refs/heads/master " + zero + "\n": false,
	} {
		cmd := exec.Command(p, "origin", "git@example.com:x.git")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantOK {
			t.Errorf("%q: err=%v out=%s", in, err, out)
		}
		if !wantOK && !strings.Contains(string(out), "no ship.yaml") {
			t.Errorf("%q: message %s", in, out)
		}
	}

	// With ship.yaml the gate runs, here a stand-in for ship (SHIP) that receives the refs and the arguments.
	if err := os.WriteFile(filepath.Join(dir, "ship.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(t.TempDir(), "ship")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"ran $*\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	push := func() ([]byte, error) {
		cmd := exec.Command(p, "origin", "url")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SHIP_CMD="+stub)
		cmd.Stdin = strings.NewReader("refs/heads/master " + head + " refs/heads/master " + zero + "\n")
		return cmd.CombinedOutput()
	}
	// Untracked, the branch does not have it: refused.
	if out, err := push(); err == nil || !strings.Contains(string(out), "no ship.yaml") {
		t.Fatalf("untracked ship.yaml accepted: %v %s", err, out)
	}
	if _, err := gitx.Git(dir, "add", "ship.yaml"); err != nil {
		t.Fatal(err)
	}
	out, err := push()
	if err != nil || !strings.Contains(string(out), "ran pre-push origin url") || !strings.Contains(string(out), "refs/heads/master "+head) {
		t.Fatalf("ship not run with refs and args: %v %s", err, out)
	}
}

// Real pushes, so the hook gets whatever environment git really gives it: from a linked worktree (GIT_DIR set,
// absolute), with --git-dir=.git --work-tree=. (both relative) and plainly. ship, here a stand-in go, must see
// the whole tree, unchanged, also from the module dir (backend/) it may work in.
func TestScript_whenRealPushes_thenModuleInSubdirSeesWholeTree(t *testing.T) {
	// The developer's global and system config (hooksPath, url rewrites, templates) must not reach these pushes.
	src := repo(t)
	shipOnGitHub(t, src)
	master, _ := gitx.Head(src)
	dir := repo(t)
	if err := os.MkdirAll(filepath.Join(dir, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, s := range map[string]string{"README.md": "x\n", "backend/go.mod": "module m\n", "ship.yaml": "go:\n  dir: backend\n"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	wt := filepath.Join(t.TempDir(), "wt")
	for _, args := range [][]string{
		{"add", "-A"}, {"commit", "-q", "-m", "module"},
		{"init", "-q", "--bare", remote}, {"remote", "add", "origin", remote},
		{"worktree", "add", "-q", "-b", "wt", wt},
	} {
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fakeGo := "#!/bin/sh\necho \"go $*\"\ncd backend || exit 1\necho \"top=$(git rev-parse --show-toplevel)\"\necho \"dirty=$(git status --short | wc -l | tr -d ' ')\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o755); err != nil {
		t.Fatal(err)
	}
	// No SHIP_CMD override here (ship's own CI exports SHIP): the hook's default go run must reach the stand-in go.
	env := gitx.CleanEnv("SHIP_CMD=", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for name, tc := range map[string]struct {
		top    string
		branch string
		push   []string
	}{
		"linked worktree": {wt, "wt", []string{"push", "-q", "origin", "wt"}},
		"relative dirs":   {dir, "master", []string{"--git-dir=.git", "--work-tree=.", "push", "-q", "origin", "master"}},
		"plain":           {dir, "master", []string{"push", "-q", "origin", "master"}},
	} {
		if _, err := gitx.Git(tc.top, "commit", "-q", "--allow-empty", "-m", name); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", tc.push...)
		cmd.Dir, cmd.Env = tc.top, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		if want := "top=" + gitx.Resolve(tc.top); !strings.Contains(string(out), want) || !strings.Contains(string(out), "dirty=0") {
			t.Errorf("%s: want %s and dirty=0, got:\n%s", name, want, out)
		}
		if want := "go run github.com/trick77/ship@" + master + " pre-push origin"; !strings.Contains(string(out), want) {
			t.Errorf("%s: want %q (master's sha), got:\n%s", name, want, out)
		}
	}
}

// Master cannot be resolved: no such repo (git says why) or no master there. The push is refused before go runs.
func TestScript_whenMasterUnresolved_thenRefused(t *testing.T) {
	dir := repo(t)
	if err := os.WriteFile(filepath.Join(dir, "ship.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(dir, "add", "ship.yaml"); err != nil {
		t.Fatal(err)
	}
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Head(dir)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho ran go\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	if _, err := gitx.Git(empty, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"no repo": filepath.Join(t.TempDir(), "missing"), "no master": empty} {
		shipOnGitHub(t, src)
		cmd := exec.Command(p, "origin", "url")
		cmd.Dir = dir
		cmd.Env = gitx.CleanEnv("SHIP_CMD=", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader("refs/heads/master " + head + " refs/heads/master " + zero + "\n")
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "cannot resolve ship's master") || strings.Contains(string(out), "ran go") {
			t.Errorf("%s: %v: %s", name, err, out)
		}
	}
}

// A bare or mirror clone has no work tree: git runs its hooks with GIT_DIR=. there. Deletes and tags have nothing
// to check and must pass before the script asks git for a top of the tree it does not have.
func TestScript_whenBareCloneDeletesOrTags_thenPass(t *testing.T) {
	dir := repo(t)
	bare := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gitx.Git(dir, "clone", "-q", "--bare", dir, bare); err != nil {
		t.Fatal(err)
	}
	p, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := gitx.Head(dir)
	for _, in := range []string{
		"(delete) " + zero + " refs/heads/x " + head + "\n",
		"refs/tags/v1 " + head + " refs/tags/v1 " + zero + "\n",
	} {
		cmd := exec.Command(p)
		cmd.Dir = bare
		cmd.Env = append(gitx.CleanEnv(), "GIT_DIR=.")
		cmd.Stdin = strings.NewReader(in)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%q: %v: %s", in, err, out)
		}
	}
}
