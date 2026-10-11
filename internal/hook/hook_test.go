package hook

import (
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

// The installed script: deletes and tags pass, a branch without ship in go.mod is refused.
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
		if !wantOK && !strings.Contains(string(out), "no ship tool") {
			t.Errorf("%q: message %s", in, out)
		}
	}

	// ship's own module line or a require entry is not the tool directive; the module may live in any tracked dir.
	push := "refs/heads/master " + head + " refs/heads/master " + zero + "\n"
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	for gomod, isTool := range map[string]bool{
		"module github.com/trick77/ship\n":                                       false,
		"module x\nrequire github.com/trick77/ship v0.1.0\n":                     false,
		"module x\ntool github.com/trick77/ship\n":                               true,
		"module x\ntool github.com/trick77/ship // gate\n":                       true,
		"module x\ntool (\n\tgithub.com/x/other\n\tgithub.com/trick77/ship\n)\n": true,
	} {
		if err := os.WriteFile(filepath.Join(dir, "server", "go.mod"), []byte(gomod), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := gitx.Git(dir, "add", "server/go.mod"); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(p)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(push)
		out, _ := cmd.CombinedOutput()
		if found := !strings.Contains(string(out), "no ship tool"); found != isTool {
			t.Errorf("%q: tool found=%v, out %s", gomod, found, out)
		}
	}
}
