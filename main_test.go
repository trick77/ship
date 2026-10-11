package main

import (
	"os"
	"strings"
	"testing"
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
	for _, args := range [][]string{{"check"}, {"release", "minor", "-dry-run"}} {
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
		args        []string
		bump, cache string
		dry         bool
	}{
		{nil, "patch", "", false},
		{[]string{"minor"}, "minor", "", false},
		{[]string{"-dry-run", "minor"}, "minor", "", true},
		{[]string{"major", "-cache", "gha", "-dry-run"}, "major", "gha", true},
		{[]string{"-cache", "gha", "major"}, "major", "gha", false},
	} {
		o, err := releaseOptions(tc.args)
		if err != nil || o.Bump != tc.bump || o.Cache != tc.cache || o.DryRun != tc.dry {
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

// git runs hooks of a linked worktree with GIT_DIR set and no GIT_WORK_TREE: git then takes the current directory
// as the top of the tree, and from a module in backend/ every file outside it read as deleted, refusing every push.
func TestRun_whenPrePushFromSubdirWithGitDir_thenTreeSeenWhole(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "master"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		if out, err := gitIn(dir, args...); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	if err := os.MkdirAll(dir+"/backend", 0o755); err != nil {
		t.Fatal(err)
	}
	for p, s := range map[string]string{"README.md": "x\n", "backend/go.mod": "module m\n"} {
		if err := os.WriteFile(dir+"/"+p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := gitIn(dir, "add", "-A"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := gitIn(dir, "commit", "-q", "-m", "one"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	head, err := gitIn(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir + "/backend")
	t.Setenv("GIT_DIR", dir+"/.git")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("refs/heads/master " + strings.TrimSpace(string(head)) + " refs/heads/master 0000000000000000000000000000000000000000\n")
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	err = run([]string{"pre-push"})
	if err == nil || strings.Contains(err.Error(), "uncommitted") || !strings.Contains(err.Error(), "ship.yaml") {
		t.Fatalf("want the ship.yaml error past a clean tree check, got: %v", err)
	}
}
