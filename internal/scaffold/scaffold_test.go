package scaffold

import (
	"bytes"
	"fmt"
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
	want := []string{CI, Release, Cleanup, Dependabot, Ignore, "ui/" + ESLint, "ui/" + TSConfigNode, "ui/" + NPMRC, Makefile,
		GitIgnore, Agents}
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
	for _, s := range []string{"directory: /backend", "directory: /ui", "package-ecosystem: docker",
		"actions-non-major:", "timezone: Europe/Zurich"} {
		if !strings.Contains(dep, s) {
			t.Errorf("dependabot lacks %q:\n%s", s, dep)
		}
	}
	if ig := string(byPath(t, files, Ignore).Want); !strings.Contains(ig, "*\n!backend\n!ui\n") ||
		!strings.Contains(ig, "ui/node_modules\nui/dist\n") {
		t.Errorf(".dockerignore:\n%s", ig)
	}
	if i, j := strings.Index(dep, "    groups:\n      # First"), strings.Index(dep, "\n\n  # Base images"); i < 0 || j < i ||
		dep[i:j+1] != npmGroups {
		t.Errorf("dependabot npm groups:\n%s", dep)
	}
	for p, want := range map[string]string{ESLint: eslintConfig, TSConfigNode: tsconfigNode, NPMRC: "save-exact=true\n"} {
		if got := string(byPath(t, files, "ui/"+p).Want); got != want {
			t.Errorf("ui/%s:\n%s", p, got)
		}
	}
}

func TestFiles_whenPublicLibrary_thenCallersOnly(t *testing.T) {
	dir := t.TempDir()
	files, err := Files(dir, parse(t, ""), Facts{Repo: "webfetch", Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(files); strings.Join(got, " ") != strings.Join([]string{CI, Release, Dependabot, GitIgnore, Agents}, " ") {
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

func TestFiles_whenGoModules_thenGomodEntryEach(t *testing.T) {
	files, err := Files(t.TempDir(), parse(t, "go:\n  modules: [kit, ./kit/auth/]\n"), Facts{Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	dep := string(byPath(t, files, Dependabot).Want)
	for _, s := range []string{"gomod\n    directory: /\n", "gomod\n    directory: /kit\n", "gomod\n    directory: /kit/auth\n"} {
		if !strings.Contains(dep, s) {
			t.Errorf("dependabot lacks %q:\n%s", s, dep)
		}
	}
}

func TestFiles_whenDependabotIgnore_thenUnderItsEcosystem(t *testing.T) {
	c := parse(t, "go:\n  dir: backend\n  modules: [kit]\nui: {}\ndependabot:\n  ignore:\n"+
		"    - { ecosystem: gomod, dependency: github.com/ncruces/go-sqlite3, why: bindings welded to v0.23.3 }\n"+
		"    - { ecosystem: npm, dependency: mermaid, versions: [\">=12.1.0\", \"<13\"], why: drops a diagram type }\n")
	files, err := Files(t.TempDir(), c, Facts{Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	dep := string(byPath(t, files, Dependabot).Want)
	gomod := "          - major\n    ignore:\n      # bindings welded to v0.23.3\n" +
		"      - dependency-name: \"github.com/ncruces/go-sqlite3\"\n\n"
	// go.dir's block and the nested module's: ship.yaml names a dependency, not a directory.
	if strings.Count(dep, gomod) != 2 {
		t.Errorf("gomod ignore, want it under both gomod entries:\n%s", dep)
	}
	npm := "        versions: [\">=6.1.0\"]\n      # drops a diagram type\n" +
		"      - dependency-name: \"mermaid\"\n        versions: [\">=12.1.0\", \"<13\"]\n"
	if !strings.Contains(dep, npm) {
		t.Errorf("npm ignore, want it after the typescript cap:\n%s", dep)
	}
	if strings.Count(dep, "ignore:") != 3 {
		t.Errorf("ignore blocks, want gomod twice and npm once:\n%s", dep)
	}
}

func TestFiles_whenIgnoreForAbsentEcosystem_thenError(t *testing.T) {
	// No ui:, no Containerfile, no compose pin: an ignore there would be written nowhere while init stays green.
	for _, eco := range []string{"npm", "docker", "docker-compose"} {
		c := parse(t, "dependabot:\n  ignore:\n    - { ecosystem: "+eco+", dependency: x, why: y }\n")
		if _, err := Files(t.TempDir(), c, Facts{Visibility: "public"}); err == nil {
			t.Errorf("%s ignore without an entry accepted", eco)
		}
	}
}

func TestFiles_whenComposePins_thenComposeEntry(t *testing.T) {
	for _, compose := range []bool{false, true} {
		files, err := Files(t.TempDir(), parse(t, ""), Facts{Visibility: "private", Compose: compose})
		if err != nil {
			t.Fatal(err)
		}
		dep := string(byPath(t, files, Dependabot).Want)
		entry := "  - package-ecosystem: docker-compose\n    directory: /\n"
		if strings.Contains(dep, entry) != compose || compose && !strings.Contains(dep, "      compose-major:") {
			t.Errorf("compose %v, dependabot:\n%s", compose, dep)
		}
	}
}

func TestFiles_whenUIElsewhere_thenInputsAndOwnedFilesThere(t *testing.T) {
	dir := t.TempDir()
	c := parse(t, "ui:\n  dir: web\n")
	files, err := Files(dir, c, Facts{Visibility: "public", NodeVersion: "27"})
	if err != nil {
		t.Fatal(err)
	}
	ci := string(byPath(t, files, CI).Want)
	if !strings.Contains(ci, "node-version: \"27\"\n      ui-dir: web\n") || strings.Contains(ci, "go-dir") {
		t.Errorf("ci.yaml:\n%s", ci)
	}
	for _, p := range []string{ESLint, TSConfigNode, NPMRC} {
		byPath(t, files, "web/"+p)
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
	if err != nil || strings.Join(wrote, " ") != strings.Join([]string{CI, Release, Dependabot, GitIgnore, Agents}, " ") {
		t.Fatalf("wrote %v %v", wrote, err)
	}
}

// AGENTS.md block lines, per variant.
const (
	agentsHead = "<!-- ship:begin -->\n<!-- Owned by ship init (github.com/trick77/ship), never edited by hand: " +
		"repo-specific lines go outside the markers. -->\n## ship\n\n"
	agentsMake = "- Gate: `make check` before every push, green. `make hooks` once per clone and again when ship says the hook is\n" +
		"  stale: the pre-push hook runs the gate. GitHub is never the first to find a failure.\n"
	agentsShip = "- Gate: `ship check` (the `ship()` function in github.com/trick77/ship's README) before every push, green.\n" +
		"  `ship hooks` once per clone and again when ship says the hook is stale: the pre-push hook runs the gate. GitHub is\n" +
		"  never the first to find a failure.\n"
	agentsPrivate = "- CI: Dependabot PRs only (Actions minutes). A Dependabot PR: `%s check` on its branch before merging it.\n"
	agentsPublic  = "- CI: every PR (Dependabot and outside contributors have no pre-push hook).\n"
	agentsOwned   = "- Owned by ship, changed with `ship init`, never by hand: the `.github/workflows/` callers, `.github/dependabot.yaml`\n" +
		"  and the ship blocks in %s.gitignore and AGENTS.md.\n%s" +
		"  No `.golangci.yaml`: ship's config is the family's.\n"
	agentsKit = "- kit (github.com/trick77/ship/kit): version, `/healthz` `/readyz`, signals and graceful shutdown come from kit,\n" +
		"  never hand-rolled. Dependabot bumps it in its own group: a v0.x minor may break.\n"
	agentsRelease = "- Release: by hand only, `gh workflow run Release -f bump=auto` (patch, or the highest `Release: minor|major` line\n" +
		"  since the last tag; a minor or major needs a `Release-note: …` line too).\n"
	agentsImages = "  Images are built, scanned and signed on GitHub only, never from a laptop.\n"
	agentsTail   = "- Commit subjects + PR titles: Go style `area: what` (go.dev/doc/contribute), gated by ship; release notes group\n" +
		"  them by area.\n<!-- ship:end -->\n"
)

func TestFiles_whenAgents_thenBlockPerVariant(t *testing.T) {
	ui := "  In `ui/`: eslint.config.js, tsconfig.node.json and .npmrc.\n"
	for _, tc := range []struct {
		name, ship, visibility string
		makefile               bool
		want                   string
	}{
		{"private app", app, "private", false, agentsHead + agentsMake + fmt.Sprintf(agentsPrivate, "make") +
			fmt.Sprintf(agentsOwned, "Makefile, .dockerignore, ", ui) + agentsKit + agentsRelease + agentsImages + agentsTail},
		{"public app", "images:\n  - {}\n", "public", false, agentsHead + agentsMake + agentsPublic +
			fmt.Sprintf(agentsOwned, "Makefile, .dockerignore, ", "") + agentsKit + agentsRelease + agentsImages + agentsTail},
		{"private make library", "", "private", true, agentsHead + agentsMake + fmt.Sprintf(agentsPrivate, "make") +
			fmt.Sprintf(agentsOwned, "Makefile, ", "") + agentsRelease + agentsTail},
		{"public make library", "", "public", true, agentsHead + agentsMake + agentsPublic +
			fmt.Sprintf(agentsOwned, "Makefile, ", "") + agentsRelease + agentsTail},
		{"private library", "", "private", false, agentsHead + agentsShip + fmt.Sprintf(agentsPrivate, "ship") +
			fmt.Sprintf(agentsOwned, "", "") + agentsRelease + agentsTail},
		{"public library", "", "public", false, agentsHead + agentsShip + agentsPublic +
			fmt.Sprintf(agentsOwned, "", "") + agentsRelease + agentsTail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.makefile {
				write(t, dir, Makefile, "build:\n\tgo build ./...\n")
			}
			files, err := Files(dir, parse(t, tc.ship), Facts{Repo: "r", Visibility: tc.visibility})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(byPath(t, files, Agents).Want); got != tc.want {
				t.Errorf("AGENTS.md:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestFiles_whenGitIgnore_thenPlaywrightAndUICoverage(t *testing.T) {
	head := "# ship:begin (owned by ship init; repo-specific lines go outside the markers)\n" +
		"# Playwright MCP's snapshots and screenshots.\n/.playwright-mcp/\n"
	cover := "# The UI tests' coverage report (ui.lcov's dir); Go's lives in the git dir.\n"
	for ship, want := range map[string]string{
		"":                           head + "# ship:end\n",
		"ui: {}\n":                   head + cover + "/coverage/ui/\n# ship:end\n",
		"ui:\n  dir: backend/web\n":  head + cover + "/backend/coverage/ui/\n# ship:end\n",
		"ui:\n  lcov: lcov.info\n":   head + "# ship:end\n",
		"ui:\n  lcov: ui/cov/x.info": head + cover + "/ui/cov/\n# ship:end\n",
	} {
		files, err := Files(t.TempDir(), parse(t, ship), Facts{Visibility: "public"})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(byPath(t, files, GitIgnore).Want); got != want {
			t.Errorf("%q .gitignore:\n%s\nwant:\n%s", ship, got, want)
		}
	}
}

// AGENTS.md and .gitignore take the block last, no guard: a check: line in AGENTS.md is prose, not a target.
func TestFiles_whenAgentsAndGitIgnoreExist_thenAppendedAndReplaced(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, Agents, "# AGENTS.md\n\ncheck: run the tests")
	write(t, dir, GitIgnore, "/bin/\n")
	files, err := Files(dir, parse(t, ""), Facts{Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	ag, gi := byPath(t, files, Agents), byPath(t, files, GitIgnore)
	if ag.Manual != "" || !strings.HasPrefix(string(ag.Want), "# AGENTS.md\n\ncheck: run the tests\n\n"+MDBegin+"\n") ||
		!strings.HasSuffix(string(ag.Want), MDEnd+"\n") {
		t.Errorf("AGENTS.md: %q %s", ag.Manual, ag.Want)
	}
	if !strings.HasPrefix(string(gi.Want), "/bin/\n\n"+Begin) || !strings.HasSuffix(string(gi.Want), End+"\n") {
		t.Errorf(".gitignore:\n%s", gi.Want)
	}
	if _, err := Write(dir, files); err != nil {
		t.Fatal(err)
	}
	// A stale block is replaced in place, the repo's lines after it kept.
	stale := strings.Replace(string(ag.Want), "## ship\n", "## old\n", 1) + "\n- repo line\n"
	write(t, dir, Agents, stale)
	files, err = Files(dir, parse(t, ""), Facts{Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(byPath(t, files, Agents).Want); got != string(ag.Want)+"\n- repo line\n" {
		t.Errorf("replaced block:\n%s", got)
	}
	if gi := byPath(t, files, GitIgnore); gi.Differs() {
		t.Errorf(".gitignore differs after Write:\n%s", gi.Want)
	}
}

// kit is v0.x: its own group, before the catch-alls, in go.dir's entry and every nested module's.
const gomodGroups = `    groups:
      # kit is v0.x: a minor may break. Its own PR, never inside the
      # catch-all (first matching group wins).
      kit:
        patterns: ["github.com/trick77/ship/kit", "github.com/trick77/ship/kit/*"]
      gomod-non-major:
        patterns:
          - "*"
        update-types:
          - patch
          - minor
      gomod-major:
        patterns:
          - "*"
        update-types:
          - major

`

func TestFiles_whenGomod_thenKitGroupFirst(t *testing.T) {
	files, err := Files(t.TempDir(), parse(t, "go:\n  modules: [kit]\n"), Facts{Visibility: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if dep := string(byPath(t, files, Dependabot).Want); strings.Count(dep, gomodGroups) != 2 {
		t.Errorf("gomod groups, want them under both gomod entries:\n%s", dep)
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

// The canon the UI repos were harmonized to, copied here: a template edit changes these too.
const npmGroups = `    groups:
      # First matching group wins, so the specific groups come first and the
      # catch-alls stay last. vite, its react plugin, vitest and its coverage
      # provider are one dependency family: vitest pins a vite major range and
      # @vitest/coverage-v8 pins vitest's exact version, so bumping any one
      # alone fails npm ci on ERESOLVE (or nests a second vite). They always
      # travel together. @tailwindcss/vite peer-depends on vite's major too,
      # and pins tailwindcss exactly: split, the plugin nests its own core.
      vite-and-vitest:
        patterns: ["vite", "@vitejs/*", "vitest", "@vitest/*", "@tailwindcss/vite", "tailwindcss"]
      # Pinned exact; a release may format differently and fail the gate's
      # format:check, so it never blocks the other bumps: its own PR,
      # reformatted there.
      prettier:
        patterns: ["prettier"]
      # A new rule or recommended-set change can fail the gate's lint: its own
      # PR, findings fixed there. typescript-eslint, @eslint/js and the
      # eslint-config-*/eslint-plugin-* packages track eslint's major.
      eslint:
        patterns: ["eslint", "eslint-*", "typescript-eslint", "@eslint/*"]
      npm-non-major:
        patterns:
          - "*"
        update-types:
          - patch
          - minor
      npm-major:
        patterns:
          - "*"
        update-types:
          - major
    ignore:
      # typescript-eslint's peer dependency takes typescript <6.1.0: any newer
      # one, minor included, fails npm ci on ERESOLVE. Lift this (and
      # conform's typescriptCap) once typescript-eslint supports newer TS.
      - dependency-name: "typescript"
        versions: [">=6.1.0"]
`

const eslintConfig = `import js from "@eslint/js";
import { defineConfig } from "eslint/config";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import eslintConfigPrettier from "eslint-config-prettier";

export default defineConfig(
  js.configs.recommended,
  tseslint.configs.recommended,
  reactHooks.configs.flat.recommended,
  eslintConfigPrettier,
  {
    rules: {
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
      "@typescript-eslint/no-explicit-any": "warn",
    },
  },
  { ignores: ["dist/"] },
);
`

const tsconfigNode = `{
  "compilerOptions": {
    "composite": true,
    "strict": true,
    "skipLibCheck": true,
    "module": "ESNext",
    "moduleResolution": "bundler",
    "allowSyntheticDefaultImports": true,
    "tsBuildInfoFile": "./node_modules/.tmp/tsconfig.node.tsbuildinfo",
    "emitDeclarationOnly": true,
    "outDir": "./node_modules/.tmp/node"
  },
  "include": ["vite.config.ts"]
}
`
