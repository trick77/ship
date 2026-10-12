package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/pins"
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

func TestVuln(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	log := fakeBin(t, "go", "exit 0")
	r := runner{root: dir, c: c}
	if err := r.vuln(); err != nil {
		t.Fatal(err)
	}
	if args := readLog(t, log); args != "run golang.org/x/vuln/cmd/govulncheck@"+pins.Govulncheck+" ./...\n" {
		t.Fatalf("args: %q", args)
	}
	fakeBin(t, "go", "exit 3")
	if err := r.vuln(); err == nil {
		t.Fatal("a reported vulnerability passed")
	}
}

func TestWithPathMode_whenNoRunSection_thenError(t *testing.T) {
	if _, err := withPathMode("version: \"2\"\n"); err == nil {
		t.Fatal("config without run: accepted")
	}
}

func TestNpmAudit_whenUI_thenProdHighAuditInUIDir(t *testing.T) {
	log := fakeBin(t, "npm", `pwd >> "$(dirname "$0")/npm.log"; exit "${AUDIT_RC:-0}"`)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := runner{root: dir, c: &config.Config{}}
	if err := r.npmAudit(); err != nil || readLog(t, log) != "" {
		t.Fatalf("no ui: %v, npm ran: %q", err, readLog(t, log))
	}
	r.c.UI = &config.UI{Dir: "web"}
	if err := r.npmAudit(); err != nil {
		t.Fatal(err)
	}
	got := readLog(t, log)
	if !strings.HasPrefix(got, "audit --audit-level=high --omit=dev\n") ||
		!strings.HasSuffix(strings.TrimSpace(got), string(filepath.Separator)+"web") {
		t.Fatalf("calls: %q", got)
	}
	t.Setenv("AUDIT_RC", "1")
	if err := r.npmAudit(); err == nil {
		t.Fatal("audit failure ignored")
	}
}
