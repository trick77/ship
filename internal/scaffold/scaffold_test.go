package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
)

func parse(t *testing.T, s string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func write(t *testing.T, dir, p, s string) {
	t.Helper()
	full := filepath.Join(dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func byPath(t *testing.T, files []File, p string) File {
	t.Helper()
	for _, f := range files {
		if f.Path == p {
			return f
		}
	}
	t.Fatalf("no %s in %v", p, paths(files))
	return File{}
}

func paths(files []File) []string {
	var ps []string
	for _, f := range files {
		ps = append(ps, f.Path)
	}
	return ps
}

const app = "go:\n  dir: backend\nui: {}\nimages:\n  - {}\n  - { suffix: -ui, target: core-ui }\n"

// ipverse-core's CI caller, the private shape, as the consumers had it before ship owned it.
const privateCI = `name: CI

# Dependabot PRs only (Actions minutes): own pushes are gated by the pre-push hook, a bot PR has none.
# By hand: gh workflow run CI --ref <branch>
on:
  pull_request:
    branches: [master]
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  check:
    if: github.event_name != 'pull_request' || github.event.pull_request.user.login == 'dependabot[bot]'
    uses: trick77/ship/.github/workflows/check.yaml@master
    with:
      go-dir: backend
      node-version: "26"
`

const publicLibCI = `name: CI

# Every pull request, fork PRs included (read-only token, no secrets): Dependabot and outside contributors have no
# pre-push hook. Own pushes are gated by the hook already. By hand: gh workflow run CI --ref <branch>
on:
  pull_request:
    branches: [master]
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  check:
    uses: trick77/ship/.github/workflows/check.yaml@master
`

func TestFiles_whenApp_thenEveryOwnedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Containerfile", "FROM scratch\n")
	files, err := Files(dir, parse(t, app), Facts{Repo: "core", Visibility: "private", DockerDirs: []string{"/"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{CI, Release, Cleanup, Dependabot, Ignore, "ui/" + ESLint, Makefile}
	if got := paths(files); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("files %v", got)
	}
	if ci := byPath(t, files, CI); string(ci.Want) != privateCI || ci.Exists || !ci.Differs() {
		t.Errorf("ci.yaml:\n%s", ci.Want)
	}
	rel := string(byPath(t, files, Release).Want)
	for _, s := range []string{"options: [patch, minor, major, auto]", "go-dir: backend", "packages: write", "id-token: write"} {
		if !strings.Contains(rel, s) {
			t.Errorf("release.yaml lacks %q:\n%s", s, rel)
		}
	}
	if cl := string(byPath(t, files, Cleanup).Want); !strings.Contains(cl, `packages: '["core", "core-ui"]'`) {
		t.Errorf("cleanup:\n%s", cl)
	}
	dep := string(byPath(t, files, Dependabot).Want)
	for _, s := range []string{"directory: /backend", "directory: /ui", "package-ecosystem: docker", "prettier:",
		"eslint:", "typescript:", `versions: [">=7"]`, "npm-major:", "actions-non-major:", "timezone: Europe/Zurich"} {
		if !strings.Contains(dep, s) {
			t.Errorf("dependabot lacks %q:\n%s", s, dep)
		}
	}
	if ig := string(byPath(t, files, Ignore).Want); !strings.Contains(ig, "*\n!backend\n!ui\n") ||
		!strings.Contains(ig, "ui/node_modules\nui/dist\n") {
		t.Errorf(".dockerignore:\n%s", ig)
	}
	if es := string(byPath(t, files, "ui/"+ESLint).Want); strings.Contains(es, "react-hooks") {
		t.Errorf("hooks without the plugin:\n%s", es)
	}
}

func TestFiles_whenPublicLibrary_thenCallersOnly(t *testing.T) {
	dir := t.TempDir()
	files, err := Files(dir, parse(t, ""), Facts{Repo: "webfetch", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(files); strings.Join(got, " ") != strings.Join([]string{CI, Release, Dependabot}, " ") {
		t.Fatalf("files %v", got)
	}
	if ci := byPath(t, files, CI); string(ci.Want) != publicLibCI {
		t.Errorf("ci.yaml:\n%s", ci.Want)
	}
	rel := string(byPath(t, files, Release).Want)
	if strings.Contains(rel, "packages:") || strings.Contains(rel, "go-dir") {
		t.Errorf("library release.yaml:\n%s", rel)
	}
	if dep := string(byPath(t, files, Dependabot).Want); strings.Contains(dep, "npm") || strings.Contains(dep, "docker") ||
		!strings.Contains(dep, "directory: /\n") {
		t.Errorf("dependabot:\n%s", dep)
	}
}

func TestFiles_whenUIElsewhereWithHooks_thenInputsAndPlugin(t *testing.T) {
	dir := t.TempDir()
	c := parse(t, "ui:\n  dir: web\n")
	files, err := Files(dir, c, Facts{Visibility: "public", ReactHooks: true})
	if err != nil {
		t.Fatal(err)
	}
	ci := string(byPath(t, files, CI).Want)
	if !strings.Contains(ci, "node-version: \"26\"\n      ui-dir: web\n") || strings.Contains(ci, "go-dir") {
		t.Errorf("ci.yaml:\n%s", ci)
	}
	if es := string(byPath(t, files, "web/"+ESLint).Want); !strings.Contains(es, "reactHooks.configs.flat.recommended") {
		t.Errorf("eslint:\n%s", es)
	}
	if mk := byPath(t, files, Makefile); !strings.HasPrefix(string(mk.Want), Begin) {
		t.Errorf("Makefile:\n%s", mk.Want)
	}
}

func TestFiles_whenFactsMissing_thenManual(t *testing.T) {
	files, err := Files(t.TempDir(), parse(t, "images:\n  - {}\n"), Facts{})
	if err != nil {
		t.Fatal(err)
	}
	if f := byPath(t, files, CI); !strings.Contains(f.Manual, "visibility unknown") || !f.Differs() {
		t.Errorf("ci: %+v", f)
	}
	if f := byPath(t, files, Cleanup); !strings.Contains(f.Manual, "origin") {
		t.Errorf("cleanup: %+v", f)
	}
}

func TestFiles_whenBlocks_thenRepoLinesKept(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, Makefile, "build:\n\tgo build ./...")
	write(t, dir, Ignore, "backend/web/dist\n")
	c := parse(t, "images:\n  - {}\n")
	files, err := Files(dir, c, Facts{Repo: "r", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	mk := byPath(t, files, Makefile)
	if !strings.HasPrefix(string(mk.Want), "build:\n\tgo build ./...\n\n"+Begin) || !strings.HasSuffix(string(mk.Want), End+"\n") {
		t.Errorf("Makefile:\n%s", mk.Want)
	}
	ig := byPath(t, files, Ignore)
	if !strings.HasPrefix(string(ig.Want), Begin) || !strings.HasSuffix(string(ig.Want), End+"\n\nbackend/web/dist\n") {
		t.Errorf(".dockerignore:\n%s", ig.Want)
	}
	// Written, a second render is clean, and a stale block is replaced in place.
	if _, err := Write(dir, files); err != nil {
		t.Fatal(err)
	}
	stale := strings.Replace(string(mk.Want), "lint:\n", "lint: tidy\n", 1) + "tidy:\n\tgo mod tidy\n"
	write(t, dir, Makefile, stale)
	files, err = Files(dir, c, Facts{Repo: "r", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path != Makefile && f.Differs() {
			t.Errorf("%s differs after Write", f.Path)
		}
	}
	if mk := byPath(t, files, Makefile); string(mk.Want) != string(mk.Have[:bytes.Index(mk.Have, []byte("lint: tidy"))])+
		"lint:\n\t$(SHIP_CMD) lint\n"+End+"\ntidy:\n\tgo mod tidy\n" {
		t.Errorf("replaced block:\n%s", mk.Want)
	}
}

func TestFiles_whenOwnTargetsOutsideBlock_thenManual(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, Makefile, "check:\n\tmake test\n")
	files, err := Files(dir, parse(t, ""), Facts{Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if mk := byPath(t, files, Makefile); !strings.Contains(mk.Manual, "own check") {
		t.Errorf("Makefile: %+v", mk)
	}
	wrote, err := Write(dir, files)
	if err != nil || strings.Join(wrote, " ") != strings.Join([]string{CI, Release, Dependabot}, " ") {
		t.Fatalf("wrote %v %v", wrote, err)
	}
}

func TestPrintDiff(t *testing.T) {
	files := []File{
		{Path: "same", Have: []byte("a\n"), Exists: true, Want: []byte("a\n")},
		{Path: "new", Want: []byte("x\ny\n")},
		{Path: "hand", Manual: "move it"},
		{Path: "edit", Exists: true, Have: []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n"),
			Want: []byte("1\n2\nTWO\n4\n5\n6\n7\n8\n9\n10\n11\neleven\n")},
	}
	var b strings.Builder
	if !PrintDiff(&b, files) {
		t.Fatal("no difference reported")
	}
	want := "--- /dev/null\n+++ b/new (ship init)\n@@ -0,0 +1,2 @@\n+x\n+y\n" +
		"hand: move it\n" +
		"--- a/edit\n+++ b/edit (ship init)\n@@ -1,6 +1,6 @@\n 1\n 2\n-3\n+TWO\n 4\n 5\n 6\n" +
		"@@ -9,4 +9,4 @@\n 9\n 10\n 11\n-12\n+eleven\n"
	if b.String() != want {
		t.Errorf("diff:\n%s\nwant:\n%s", b.String(), want)
	}
	if PrintDiff(&b, files[:1]) {
		t.Error("same file reported")
	}
}
