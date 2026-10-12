package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/release"
)

func TestRun_whenNoOrUnknownCommand_thenError(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("no command accepted")
	}
	if err := run([]string{"frobnicate"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v", err)
	}
	if err := run([]string{"help"}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_whenHelpOutsideRepo_thenUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, h := range []string{"help", "-h", "--help"} {
		if err := run([]string{h}); err != nil {
			t.Errorf("%s: %v", h, err)
		}
	}
}

func TestRun_whenNoShipYaml_thenCheckAndReleaseRefuse(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := run([]string{"check"}); err == nil {
		t.Fatal("check outside a repo")
	}
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, args := range [][]string{{"check"}, {"lint"}, {"release", "minor", "-dry-run"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "ship.yaml") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := run([]string{"release", "-bogus"}); err == nil {
		t.Error("bad flag accepted")
	}
}

func TestReleaseOptions(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want release.Options
	}{
		{nil, release.Options{Bump: "patch"}},
		{[]string{"minor"}, release.Options{Bump: "minor"}},
		{[]string{"-dry-run", "minor"}, release.Options{Bump: "minor", DryRun: true}},
		{[]string{"major", "-cache", "gha", "-dry-run"}, release.Options{Bump: "major", Cache: "gha", DryRun: true}},
		{[]string{"-cache", "gha", "major"}, release.Options{Bump: "major", Cache: "gha"}},
		{[]string{"auto", "-dir", "kit/auth"}, release.Options{Bump: "auto", Dir: "kit/auth"}},
	} {
		if o, err := releaseOptions(tc.args); err != nil || o != tc.want {
			t.Errorf("%v: %+v %v", tc.args, o, err)
		}
	}
	for _, args := range [][]string{{"-dry-run", "minor", "major"}, {"minor", "-dry-run", "major"}, {"-bogus"}} {
		if _, err := releaseOptions(args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestRun_whenPrePushOnlyDeletes_thenNothingToGate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("(delete) 0000000000000000000000000000000000000000 refs/heads/x 1111111111111111111111111111111111111111\n")
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if err := run([]string{"pre-push"}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_whenHooks_thenInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := run([]string{"hooks"}); err != nil {
		t.Fatal(err)
	}
}

// A hook from ship v0.2.0 in a linked worktree: git sets GIT_DIR alone, the hook cds into backend/ and runs ship
// pre-push there. ship pins the top itself: the clean tree reads clean (the ship.yaml error is the next step). That
// old hook is stale: ship warns and still gates.
func TestRun_whenOldHookFromModuleSubdirInLinkedWorktree_thenTreeSeenWhole(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := "#!/bin/sh\n# Installed by ship hooks\ncd backend && go tool ship pre-push \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-push"), []byte(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, s := range map[string]string{"README.md": "x\n", "backend/go.mod": "module m\n"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "one"},
		{"worktree", "add", "-q", "-b", "wt", wt},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, gitx.CleanEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	gd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	gd.Dir, gd.Env = wt, gitx.CleanEnv()
	gitDir, err := gd.Output()
	if err != nil {
		t.Fatal(err)
	}
	head := exec.Command("git", "rev-parse", "HEAD")
	head.Dir, head.Env = wt, gitx.CleanEnv()
	sha, err := head.Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wt, "backend"))
	t.Setenv("GIT_DIR", strings.TrimSpace(string(gitDir)))
	_ = os.Unsetenv("GIT_WORK_TREE")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("refs/heads/wt " + strings.TrimSpace(string(sha)) + " refs/heads/wt 0000000000000000000000000000000000000000\n")
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; _ = r.Close() })
	err = run([]string{"pre-push"})
	if err == nil || strings.Contains(err.Error(), "uncommitted") || !strings.Contains(err.Error(), "ship.yaml") {
		t.Fatalf("want the ship.yaml error past a clean tree check, got: %v", err)
	}
}
