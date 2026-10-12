package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin puts an executable sh script named name first on PATH and returns the file its arguments are logged to,
// one call per line.
func fakeBin(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, name+".log")
	body := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestLint_whenNoRepoConfig_thenEmbeddedOne(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	log := fakeBin(t, "go", "exit 0")
	if err := Lint(dir, c); err != nil {
		t.Fatal(err)
	}
	args := strings.TrimSpace(readLog(t, log))
	_, p, ok := strings.Cut(args, "--config ")
	if !ok || !strings.Contains(args, "golangci-lint@"+c.Lint+" run") {
		t.Fatalf("args: %s", args)
	}
	if !strings.Contains(p, string(filepath.Separator)+".git"+string(filepath.Separator)) {
		t.Fatalf("config not in the git dir: %s", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "run:\n  relative-path-mode: gomod\n  timeout: 5m\n") ||
		strings.Replace(string(b), "  relative-path-mode: gomod\n", "", 1) != golangciConfig {
		t.Fatalf("written config:\n%s", b)
	}
}

func TestLint_whenRepoConfigAboveGoDir_thenItsOwn(t *testing.T) {
	dir, c := fixture(t, "go:\n  dir: lib\n", "mode: set\n", "")
	if err := os.WriteFile(filepath.Join(dir, ".golangci.yml"), []byte("version: \"2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := fakeBin(t, "go", "exit 0")
	if err := Lint(dir, c); err != nil {
		t.Fatal(err)
	}
	if args := readLog(t, log); strings.Contains(args, "--config") {
		t.Fatalf("repo config overridden: %s", args)
	}
	fakeBin(t, "go", "exit 2")
	if err := Lint(dir, c); err == nil {
		t.Fatal("lint failure ignored")
	}
}

func TestWithPathMode_whenNoRunSection_thenError(t *testing.T) {
	if _, err := withPathMode("version: \"2\"\n"); err == nil {
		t.Fatal("config without run: accepted")
	}
}
