package check

import (
	"os"
	"os/exec"
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
	log := fakeBin(t, "npm", `pwd >> "$(dirname "$0")/npm.log"; printf '%s' "$AUDIT_OUT"; exit "${AUDIT_RC:-0}"`)
	t.Setenv("AUDIT_OUT", "{}")
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
	if !strings.HasPrefix(got, "audit --json --audit-level=high --omit=dev\n") ||
		!strings.HasSuffix(strings.TrimSpace(got), string(filepath.Separator)+"web") {
		t.Fatalf("calls: %q", got)
	}
	t.Setenv("AUDIT_RC", "1")
	t.Setenv("AUDIT_OUT", `{"vulnerabilities":{"x":{"severity":"high","via":[{"title":"T","url":"U","severity":"high"}]}}}`)
	if err := r.npmAudit(); err == nil || !strings.Contains(err.Error(), "x: high T (U)") {
		t.Fatalf("finding: %v", err)
	}
}

// npm audit --json: advisories at high or above fail listed; npm's own failure fails as npm's, not as a finding.
func TestAuditResult(t *testing.T) {
	report := `{"auditReportVersion":2,"vulnerabilities":{
	  "lodash":{"severity":"high","via":[
	    {"title":"Command Injection in lodash","url":"https://github.com/advisories/GHSA-35jh-r3h4-6jhm","severity":"high"},
	    {"title":"ReDoS in lodash","url":"https://github.com/advisories/GHSA-29mw-wpgm-hmr9","severity":"moderate"}]},
	  "minimist":{"severity":"critical","via":[{"title":"Prototype Pollution in minimist","url":"https://x/1","severity":"critical"}]},
	  "app-dep":{"severity":"high","via":["lodash"]},
	  "low":{"severity":"moderate","via":[{"title":"meh","url":"https://x/2","severity":"moderate"}]}}}`
	exit1 := &exec.ExitError{}
	err := auditResult([]byte(report), "", exit1)
	want := "app-dep: high, via lodash\n" +
		"lodash: high Command Injection in lodash (https://github.com/advisories/GHSA-35jh-r3h4-6jhm)\n" +
		"minimist: critical Prototype Pollution in minimist (https://x/1)"
	if err == nil || !strings.HasSuffix(err.Error(), want) || strings.Contains(err.Error(), "ReDoS") ||
		strings.Contains(err.Error(), "meh") || strings.Contains(err.Error(), "not a vulnerability") {
		t.Fatalf("findings: %v", err)
	}
	if err := auditResult([]byte(`{"vulnerabilities":{"low":{"severity":"moderate","via":[]}}}`), "", nil); err != nil {
		t.Fatalf("moderate only: %v", err)
	}
	for _, tt := range []struct {
		out, stderr string
		runErr      error
		want        string
	}{
		{`{"error":{"code":"ENOLOCK","summary":"This command requires an existing lockfile."}}`, "", exit1,
			"ENOLOCK: This command requires an existing lockfile."},
		{"", "npm error code EAI_AGAIN\n", exit1, "EAI_AGAIN"},
		{"garbage", "", nil, "unreadable --json output"},
		{`{}`, "npm error weird", exit1, "no high or critical advisory reported: npm error weird"},
	} {
		err := auditResult([]byte(tt.out), tt.stderr, tt.runErr)
		if err == nil || !strings.Contains(err.Error(), "not a vulnerability") || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: %v, want npm's failure with %q", tt.out, err, tt.want)
		}
	}
}
