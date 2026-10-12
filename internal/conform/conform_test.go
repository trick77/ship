package conform

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/scaffold"
)

const appShip = "go:\n  dir: backend\nui:\n  lint: npm run lint\nimages:\n  - {}\n  - { suffix: -ui, target: core-ui }\n"

const appContainerfile = `# Two images from one file.
FROM node:26-alpine AS ui
WORKDIR /ui
COPY ui/ ./
RUN npm ci && npm run build

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY backend/ ./
COPY --from=ui /ui/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      # stamped through kit
      -ldflags="-s -w -X github.com/trick77/ship/kit/version.Version=${VERSION}" \
      -o /out/app ./cmd/app

FROM gcr.io/distroless/static-debian13 AS core-ui
COPY --from=build /out/app /app
USER 65532:65532

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
`

const appPackageJSON = `{
  "scripts": {
    "dev": "vite",
    "build": "tsc -b && vite build",
    "lint": "eslint . --max-warnings 0 && npm run format:check",
    "format": "prettier --write 'src/**/*.{ts,tsx,css}' '*.ts'",
    "format:check": "prettier --check 'src/**/*.{ts,tsx,css}' '*.ts'",
    "test": "vitest run"
  },
  "devDependencies": {
    "@eslint/js": "10.0.1",
    "eslint": "10.12.0",
    "eslint-config-prettier": "10.1.8",
    "eslint-plugin-react-hooks": "7.1.1",
    "prettier": "3.9.9",
    "typescript": "6.0.3",
    "typescript-eslint": "8.71.1",
    "vite": "^8.3.4"
  }
}
`

// JSONC, as transmission-ui's: a comment, own spacing.
const appTSConfig = `{
  // vite.config.ts is typechecked through the reference.
  "compilerOptions": { "strict": true },
  "include": ["src"],
  "references": [{"path":"./tsconfig.node.json"}]
}
`

// lens's, trimmed.
const appViteConfig = `import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    coverage: {
      provider: "v8",
      reportsDirectory: "../coverage/ui",
    },
  },
});
`

// transmission-ui's shape: a leading comment, the import, the repo's stubs.
const appSetup = `// jest-dom matchers on vitest's expect.
import "@testing-library/jest-dom/vitest";

globalThis.ResizeObserver = class {} as never;
`

const agentsMD = "# AGENTS.md\n\n- Commit subjects + PR titles: Go style `area: what` (`api: …`), gated by ship; release notes list them " +
	"by area.\n"

// repo is a fixture: a git repo with a GitHub origin and the given files.
type repo struct {
	t    *testing.T
	dir  string
	out  strings.Builder
	opts Options
}

func newRepo(t *testing.T, files map[string]string) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	for _, args := range [][]string{{"init", "-q", "-b", "master"}, {"remote", "add", "origin", "git@github.com:o/app.git"}} {
		if _, err := gitx.Git(r.dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	for p, s := range files {
		r.write(p, s)
	}
	r.opts = Options{
		Out:     &r.out,
		Getenv:  fakeEnv(map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_PATH": r.event(true)}),
		KitTags: func() ([]string, error) { return []string{"kit/v0.1.0", "kit/v0.2.0", "v1.0.0", "kit/vX"}, nil },
	}
	return r
}

func fakeEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// event writes an Actions event payload outside the repo.
func (r *repo) event(private bool) string {
	p := filepath.Join(r.t.TempDir(), "event.json")
	s := `{"repository": {"private": false}}`
	if private {
		s = `{"repository": {"private": true}}`
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *repo) write(p, s string) {
	r.t.Helper()
	full := filepath.Join(r.dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) remove(p string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, p)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) edit(p, old, new string) {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, p))
	if err != nil {
		r.t.Fatal(err)
	}
	if !strings.Contains(string(b), old) {
		r.t.Fatalf("%s lacks %q", p, old)
	}
	r.write(p, strings.Replace(string(b), old, new, 1))
}

func (r *repo) config(s string) *config.Config {
	r.t.Helper()
	c, err := config.Parse([]byte(s))
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// run conforms with ship.yaml s and returns the output.
func (r *repo) run(s string) (string, error) {
	r.out.Reset()
	err := Run(r.dir, r.config(s), r.opts)
	return r.out.String(), err
}

// app is a private service with a UI and two images, conforming after init.
func app(t *testing.T) *repo {
	t.Helper()
	r := newRepo(t, map[string]string{
		"Containerfile":        appContainerfile,
		"ui/package.json":      appPackageJSON,
		"ui/tsconfig.json":     appTSConfig,
		"ui/vite.config.ts":    appViteConfig,
		"ui/src/test/setup.ts": appSetup,
		"backend/go.mod":       "module example.com/app\n\ngo 1.27.1\n\nrequire (\n\tgithub.com/trick77/ship/kit v0.2.0 // indirect\n)\n",
		"backend/cmd/app/x.go": "package main\n\nfunc main() {}\n",
		"AGENTS.md":            agentsMD,
		"Makefile":             "build:\n\tgo build ./...\n",
	})
	if err := Init(r.dir, r.config(appShip), r.opts, false); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRun_whenAppConforms_thenEveryRuleOK(t *testing.T) {
	r := app(t)
	out, err := r.run(appShip)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, rl := range rules() {
		if !strings.Contains(out, "ok "+rl.name+"\n") {
			t.Errorf("no ok %s:\n%s", rl.name, out)
		}
	}
	for _, p := range []string{scaffold.ESLint, scaffold.TSConfigNode, scaffold.NPMRC} {
		if _, err := os.Stat(filepath.Join(r.dir, "ui", p)); err != nil {
			t.Errorf("init did not write ui/%s: %v", p, err)
		}
	}
}

func TestRun_whenOneThingDrifts_thenThatRuleFails(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ship  string
		mut   func(r *repo)
		fails []string
	}{
		{"scaffold edited", appShip, func(r *repo) { r.edit(scaffold.CI, "cancel-in-progress: true", "cancel-in-progress: false") },
			[]string{"FAIL scaffold: differs from ship's template (ship init -diff) (.github/workflows/ci.yaml)"}},
		{"scaffold missing", appShip, func(r *repo) { r.remove(scaffold.Cleanup) }, []string{
			"FAIL scaffold: missing (ship init) (.github/workflows/cleanup-images.yaml)",
			"FAIL workflows: missing (ship init) (.github/workflows/cleanup-images.yaml)",
			"FAIL visibility: private repo with images: the cleanup-images caller is missing"}},
		{"caller pinned", appShip, func(r *repo) { r.edit(scaffold.CI, "check.yaml@master", "check.yaml@v1") },
			[]string{"FAIL workflows: trick77/ship/.github/workflows/check.yaml@v1: call it @master"}},
		{"caller gone", appShip, func(r *repo) { r.edit(scaffold.Release, "trick77/ship/.github/workflows/release.yaml", "./x.yaml") },
			[]string{"FAIL workflows: no job calls trick77/ship/.github/workflows/release.yaml@master"}},
		{"release perms and bumps", appShip, func(r *repo) {
			r.edit(scaffold.Release, "      packages: write\n", "")
			r.edit(scaffold.Release, ", auto]", "]")
		}, []string{"FAIL workflows: ship.yaml has images: the release job needs permissions packages: write",
			"FAIL workflows: workflow_dispatch bump options lack auto"}},
		// An allowed scaffold diff still needs the grant cosign signs with.
		{"release without id-token", appShip + "allow:\n  - { rule: scaffold, path: .github/workflows/release.yaml, why: own }\n",
			func(r *repo) { r.edit(scaffold.Release, "      id-token: write\n", "") },
			[]string{"FAIL workflows: ship.yaml has images: the release job needs permissions id-token: write (cosign)"}},
		{"release id-token without images", "go:\n  dir: backend\nui:\n  lint: npm run lint\n", func(*repo) {},
			[]string{"FAIL workflows: id-token: write without images in ship.yaml: drop it",
				"FAIL workflows: packages: write without images in ship.yaml: drop it"}},
		{"foreign cleanup", appShip, func(r *repo) {
			r.write(".github/workflows/prune.yml", "on: push\njobs:\n  x:\n    steps:\n      - uses: dataaxiom/ghcr-cleanup-action@v1\n")
		}, []string{"FAIL workflows: image cleanup by dataaxiom/ghcr-cleanup-action: use the cleanup-images caller (.github/workflows/prune.yml)"}},
		{"broken workflow", appShip, func(r *repo) { r.write(".github/workflows/x.yaml", "jobs: [\n") },
			[]string{"FAIL workflows: .github/workflows/x.yaml: yaml"}},
		{"private without if", appShip, func(r *repo) { r.edit(scaffold.CI, "    if: github", "    x-if: github") },
			[]string{"FAIL visibility: private repo: the check job needs if:"}},
		{"private ungrouped", appShip, func(r *repo) {
			r.edit(scaffold.Dependabot, "      npm-major:", "      npm-x:")
			r.edit(scaffold.Dependabot, "      - major\n    ignore", "      - premajor\n    ignore")
			r.edit(scaffold.Dependabot, "open-pull-requests-limit: 5", "open-pull-requests-limit: 10")
		}, []string{"FAIL visibility: npm /ui: not every update grouped", "FAIL visibility: gomod /backend: open-pull-requests-limit 10"}},
		{"visibility mismatch", "visibility: public\n" + appShip, func(*repo) {},
			[]string{"FAIL visibility: visibility: public, GitHub says private (ship.yaml)"}},
		{"dependabot prefix and docker", appShip, func(r *repo) {
			r.edit(scaffold.Dependabot, "{ prefix: deps }", "{ prefix: chore }")
			r.edit(scaffold.Dependabot, "package-ecosystem: docker", "package-ecosystem: dockerx")
		}, []string{`FAIL dependabot: gomod /backend: commit-message prefix "chore", want deps`,
			"FAIL dependabot: no docker entry for /"}},
		{"dependabot nested module", strings.Replace(appShip, "dir: backend\n", "dir: backend\n  modules: [lib]\n", 1),
			func(r *repo) { r.write("lib/go.mod", "module example.com/lib\n\ngo 1.27.1\n") },
			[]string{"FAIL dependabot: no gomod entry for /lib"}},
		{"dependabot yml", appShip, func(r *repo) { r.write(".github/dependabot.yml", "") },
			[]string{"FAIL dependabot: rename to dependabot.yaml"}},
		{"dependabot missing", appShip, func(r *repo) { r.remove(scaffold.Dependabot) },
			[]string{"FAIL dependabot: missing (ship init)"}},
		{"containerfile", appShip, func(r *repo) {
			r.edit("Containerfile", "-trimpath", "")
			r.edit("Containerfile", "ARG VERSION=dev\n", "")
			r.edit("Containerfile", "ship/kit/version", "app/internal/version")
			r.edit("Containerfile", "USER 65532:65532", "USER root")
			r.edit("Containerfile", "golang:1.27", "golang:1.26")
		}, []string{`FAIL containerfile: image "": no ARG VERSION`, `FAIL containerfile: image "": go build ldflags lack -X`,
			`FAIL containerfile: image "": go build without -trimpath`,
			`FAIL containerfile: image "-ui": final stage runs as root`,
			`FAIL containerfile: image "": builder golang:1.26, go.mod says go 1.27`,
			`FAIL containerfile: image "": builder golang:1.26, want at least 1.27 (pins.GoMin)`,
			// The -ui image is built from the same build stage.
			`FAIL containerfile: image "-ui": no ARG VERSION`, `FAIL containerfile: image "-ui": go build ldflags lack -X`,
			`FAIL containerfile: image "-ui": go build without -trimpath`,
			`FAIL containerfile: image "-ui": builder golang:1.26`}},
		{"containerfile no build", appShip, func(r *repo) { r.write("Containerfile", "FROM gcr.io/distroless/static:nonroot\n") },
			[]string{`FAIL containerfile: image "": no RUN go build`, `FAIL containerfile: image "-ui": no stage "core-ui"`}},
		// A global ARG reaches no stage that does not redeclare it: ${VERSION} would build empty.
		{"containerfile global arg", appShip, func(r *repo) {
			r.edit("Containerfile", "ARG VERSION=dev\n", "")
			r.edit("Containerfile", "# Two images from one file.\n", "ARG VERSION=dev\n")
		}, []string{`FAIL containerfile: image "": ARG VERSION before the first FROM reaches no stage: redeclare it in the stage running go build`,
			`FAIL containerfile: image "-ui": ARG VERSION before the first FROM reaches no stage`}},
		{"containerfile missing", appShip, func(r *repo) { r.remove("Containerfile") }, []string{
			`FAIL containerfile: image "": missing (Containerfile)`, `FAIL containerfile: image "-ui": missing (Containerfile)`}},
		{"bad target", strings.Replace(appShip, "target: core-ui", "target: nope", 1), func(*repo) {},
			[]string{`FAIL containerfile: image "-ui": no stage "nope"`}},
		{"distroless", appShip, func(r *repo) {
			r.edit("Containerfile", "FROM gcr.io/distroless/static-debian13:nonroot", "FROM alpine:3.24")
		},
			[]string{`FAIL distroless: image "": final stage base alpine:3.24, not gcr.io/distroless/ (Containerfile)`,
				`FAIL containerfile: image "": final stage runs as root`}},
		{"dockerignore", appShip, func(r *repo) { r.write(scaffold.Ignore, "*\n") },
			[]string{"FAIL dockerignore: does not exclude ui/node_modules", "FAIL dockerignore: does not exclude ui/dist"}},
		{"dockerignore missing", appShip, func(r *repo) { r.remove(scaffold.Ignore) },
			[]string{"FAIL dockerignore: missing (ship init) (.dockerignore)"}},
		{"ui", strings.Replace(appShip, "npm run lint", "npm run format:check", 1), func(r *repo) {
			r.write("ui/package.json", `{"scripts": {"build": "tsc --noEmit && vite build", "lint": "eslint ."},
				"devDependencies": {"prettier": "^3.9.9"}}`)
			r.remove("ui/" + scaffold.ESLint)
		}, []string{`FAIL ui: ui.lint "npm run format:check", want npm run lint (ship.yaml)`,
			`FAIL ui: script lint "eslint .", want`, `FAIL ui: script format:check "", want`,
			`FAIL ui: script build "tsc --noEmit && vite build", want "tsc -b && vite build"`,
			`FAIL ui: script format "", want`, `FAIL ui: script test "", want "vitest run"`,
			`FAIL ui: devDependency prettier "^3.9.9": want an exact version (.npmrc save-exact) (ui/package.json)`,
			"FAIL ui: devDependency typescript missing", "FAIL ui: devDependency eslint missing",
			"FAIL ui: devDependency typescript-eslint missing", "FAIL ui: devDependency eslint-config-prettier missing",
			"FAIL ui: devDependency @eslint/js missing", "FAIL ui: devDependency eslint-plugin-react-hooks missing",
			"FAIL ui: missing (ship init) (ui/eslint.config.js)"}},
		// Dependabot moves a version per repo: any exact one passes, a range or TypeScript 6.1+ does not.
		{"ui devDependency versions", appShip, func(r *repo) {
			r.edit("ui/package.json", `"eslint": "10.12.0"`, `"eslint": "~10.13.0"`)
			r.edit("ui/package.json", `"typescript": "6.0.3"`, `"typescript": "6.1.0"`)
			r.edit("ui/package.json", `"prettier": "3.9.9"`, `"prettier": "3.10.0"`)
		}, []string{`FAIL ui: devDependency eslint "~10.13.0": want an exact version`,
			"FAIL ui: devDependency typescript 6.1.0: want below 6.1, typescript-eslint does not support it"}},
		{"ui script extra flag", appShip, func(r *repo) { r.edit("ui/package.json", `"vitest run"`, `"vitest run --passWithNoTests"`) },
			[]string{`FAIL ui: script test "vitest run --passWithNoTests", want "vitest run"`}},
		{"ui tsconfig without reference", appShip, func(r *repo) {
			r.edit("ui/tsconfig.json", `"./tsconfig.node.json"`, `"./tsconfig.app.json"`)
		}, []string{`FAIL ui: no reference to ./tsconfig.node.json`}},
		{"ui tsconfig missing", appShip, func(r *repo) { r.remove("ui/tsconfig.json") },
			[]string{"FAIL ui: missing (ui/tsconfig.json)"}},
		{"ui vite config", appShip, func(r *repo) {
			r.edit("ui/vite.config.ts", `from "vitest/config"`, `from "vite"`)
			r.edit("ui/vite.config.ts", "globals: true", "globals: false")
			r.edit("ui/vite.config.ts", "./src/test/setup.ts", "./src/setupTests.ts")
			r.edit("ui/vite.config.ts", "../coverage/ui", "coverage")
		}, []string{`FAIL ui: no from "vitest/config"`, "FAIL ui: no globals: true",
			`FAIL ui: no setupFiles: ["./src/test/setup.ts"]`, `FAIL ui: no reportsDirectory: "../coverage/ui"`}},
		{"ui vite config missing", appShip, func(r *repo) { r.remove("ui/vite.config.ts") },
			[]string{"FAIL ui: missing (ui/vite.config.ts)"}},
		{"ui setup import late", appShip, func(r *repo) {
			r.write("ui/src/test/setup.ts", "import { vi } from \"vitest\";\n"+appSetup)
		}, []string{`FAIL ui: first statement is not import "@testing-library/jest-dom/vitest"; (ui/src/test/setup.ts)`}},
		{"ui setup missing", appShip, func(r *repo) { r.remove("ui/src/test/setup.ts") },
			[]string{"FAIL ui: missing (first statement import"}},
		{"ui npmrc", appShip, func(r *repo) { r.write("ui/.npmrc", "save-exact=false\n") },
			[]string{"FAIL scaffold: differs from ship's template (ship init -diff) (ui/.npmrc)"}},
		{"ui no package.json", appShip, func(r *repo) { r.remove("ui/package.json") },
			[]string{"FAIL ui: missing (ui/package.json)"}},
		{"ui bad package.json", appShip, func(r *repo) { r.write("ui/package.json", "{") },
			[]string{"FAIL ui: ui/package.json: unexpected end of JSON input"}},
		{"golangci", appShip, func(r *repo) { r.write("backend/.golangci.yml", "") },
			[]string{"FAIL golangci: own golangci-lint config: ship's is the family's (backend/.golangci.yml)"}},
		{"gomod", appShip, func(r *repo) { r.edit("backend/go.mod", "go 1.27.1", "go 1.27") },
			[]string{"FAIL gomod: go 1.27: want x.y.z (backend/go.mod)"}},
		{"kit old", appShip, func(r *repo) { r.edit("backend/go.mod", "kit v0.2.0", "kit v0.1.0") },
			[]string{"FAIL gomod: github.com/trick77/ship/kit v0.1.0, newest is v0.2.0"}},
		{"kit missing", appShip, func(r *repo) { r.edit("backend/go.mod", "github.com/trick77/ship/kit", "example.com/other") },
			[]string{"FAIL kit: a service (images) requires github.com/trick77/ship/kit (backend/go.mod)"}},
		// Below the floor, go.mod and builder agreeing: both fail.
		{"go below floor", appShip, func(r *repo) {
			r.edit("backend/go.mod", "go 1.27.1", "go 1.26.4")
			r.edit("Containerfile", "golang:1.27", "golang:1.26")
		}, []string{"FAIL gomod: go 1.26.4: want at least go 1.27.0 (pins.GoMin) (backend/go.mod)",
			`FAIL containerfile: image "": builder golang:1.26, want at least 1.27`,
			`FAIL containerfile: image "-ui": builder golang:1.26, want at least 1.27`}},
		{"go.modules", strings.Replace(appShip, "dir: backend\n", "dir: backend\n  modules: [backend/tools, lib]\n", 1), func(r *repo) {
			r.write("backend/tools/go.mod", "module tools\n\ngo 1.26.0\n")
		}, []string{"FAIL gomod: go 1.26.0: want at least go 1.27.0 (pins.GoMin) (backend/tools/go.mod)",
			"FAIL gomod: missing (ship.yaml go.modules) (lib/go.mod)",
			"FAIL dependabot: no gomod entry for /backend/tools", "FAIL dependabot: no gomod entry for /lib"}},
		{"node below floor", appShip, func(r *repo) { r.edit("Containerfile", "node:26-alpine", "node:24-alpine") },
			[]string{`FAIL containerfile: image "": builder node:24, want at least 26 (pins.NodeMin)`,
				`FAIL containerfile: image "-ui": builder node:24, want at least 26 (pins.NodeMin)`}},
		// Dependabot moved the node builder up: the CI caller follows it (ship init), never a hand edit.
		{"node bumped", appShip, func(r *repo) { r.edit("Containerfile", "node:26-alpine", "node:27-alpine") },
			[]string{"FAIL scaffold: differs from ship's template (ship init -diff) (.github/workflows/ci.yaml)"}},
		// An unnumbered tag would pass every floor unseen.
		{"builders unnumbered", appShip, func(r *repo) {
			r.edit("Containerfile", "node:26-alpine", "node:lts-alpine")
			r.edit("Containerfile", "golang:1.27-alpine", "golang:alpine")
		}, []string{`FAIL containerfile: image "": builder node:lts-alpine names no node version`,
			`FAIL containerfile: image "": builder golang:alpine names no golang version`,
			`FAIL containerfile: image "-ui": builder node:lts-alpine names no node version`,
			`FAIL containerfile: image "-ui": builder golang:alpine names no golang version`}},
		{"go.mod missing", appShip, func(r *repo) { r.remove("backend/go.mod") },
			[]string{"FAIL gomod: missing (backend/go.mod)"}},
		{"handrolled", appShip, func(r *repo) {
			r.write("backend/internal/version/version.go", "package version\n\nvar Version = \"dev\"\n")
			r.write("backend/internal/api/api.go", "package api\n\nconst p = \"GET /api/v1/"+"health\"\n")
			r.write("backend/cmd/app/main.go", "package main\n\nfunc init() { signal."+"NotifyContext(nil) }\n")
			r.write("backend/cmd/app/main_test.go", "package main\n\nconst p = \"/"+"health\"\n")
			r.write("backend/testdata/x.go", "package x\n\nconst p = \"/"+"health\"\n")
			r.write("backend/tools/go.mod", "module tools\n")
			r.write("backend/tools/x.go", "package x\n\nconst p = \"/"+"health\"\n")
		}, []string{"FAIL handrolled: own version package: use github.com/trick77/ship/kit/version (backend/internal/version)",
			"FAIL handrolled: own health route: use kit's (backend/internal/api/api.go)",
			"FAIL handrolled: own shutdown signal handling: use kit's (backend/cmd/app/main.go)"}},
		{"agents", appShip, func(r *repo) { r.write("AGENTS.md", "# AGENTS.md\n") },
			[]string{"FAIL agents: no commit-subject line"}},
		{"agents missing", appShip, func(r *repo) { r.remove("AGENTS.md") },
			[]string{"FAIL agents: missing (AGENTS.md)"}},
		{"unknown allow", appShip + "allow:\n  - { rule: scafold, why: typo }\n", func(*repo) {},
			[]string{`FAIL allow: "scafold" names no conform rule (ship.yaml)`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := app(t)
			tc.mut(r)
			out, err := r.run(tc.ship)
			if err == nil || !strings.Contains(err.Error(), " failed (") {
				t.Errorf("err = %v", err)
			}
			for _, f := range tc.fails {
				if !strings.Contains(out, f) {
					t.Errorf("no %q in:\n%s", f, out)
				}
			}
			// Every FAIL is expected, but an owned file edited by hand also fails scaffold.
			for _, l := range strings.Split(out, "\n") {
				if !strings.HasPrefix(l, "FAIL ") || strings.HasPrefix(l, "FAIL scaffold: ") {
					continue
				}
				if !slices.ContainsFunc(tc.fails, func(f string) bool { return strings.Contains(l, f) }) {
					t.Errorf("unexpected %q", l)
				}
			}
		})
	}
}

func TestRun_whenAllowed_thenWhyPrinted(t *testing.T) {
	r := app(t)
	r.edit("Containerfile", "USER 65532:65532", "USER 0")
	r.edit("Containerfile", "FROM gcr.io/distroless/static-debian13 AS core-ui", "FROM alpine:3.24 AS core-ui")
	r.write("backend/.golangci.yaml", "")
	r.edit(scaffold.CI, "cancel-in-progress: true", "cancel-in-progress: false")
	ship := appShip + "allow:\n" +
		"  - { rule: containerfile, image: -ui, why: smartctl needs root }\n" +
		"  - { rule: distroless, image: -ui, why: busybox healthcheck }\n" +
		"  - { rule: golangci, path: backend/.golangci.yaml, why: legacy lint }\n" +
		"  - { rule: scaffold, path: .github/workflows/ci.yaml, why: long CI }\n"
	out, err := r.run(ship)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, s := range []string{"allowed containerfile: smartctl needs root\n", "allowed distroless: busybox healthcheck\n",
		"allowed golangci: legacy lint\n", "allowed scaffold: long CI\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("no %q in:\n%s", s, out)
		}
	}
}

func TestRun_whenLibrary_thenAppRulesSkipped(t *testing.T) {
	r := newRepo(t, map[string]string{"go.mod": "module example.com/lib\n\ngo 1.27.1\n", "lib.go": "package lib\n",
		"AGENTS.md": agentsMD})
	r.opts.Getenv = fakeEnv(map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_PATH": r.event(false)})
	r.opts.KitTags = func() ([]string, error) { return nil, errors.New("not asked") }
	if err := Init(r.dir, r.config(""), r.opts, false); err != nil {
		t.Fatal(err)
	}
	if r.out.String() != "ship: wrote .github/workflows/ci.yaml\nship: wrote .github/workflows/release.yaml\n"+
		"ship: wrote .github/dependabot.yaml\n" {
		t.Errorf("init:\n%s", r.out.String())
	}
	out, err := r.run("")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, s := range []string{"skip containerfile: no images", "skip distroless: no images", "skip dockerignore: no images",
		"skip ui: no ui", "skip kit: no images", "ok scaffold", "ok visibility"} {
		if !strings.Contains(out, s+"\n") {
			t.Errorf("no %q in:\n%s", s, out)
		}
	}
	r.edit(scaffold.CI, "  check:\n", "  check:\n    if: false\n")
	if out, _ := r.run(""); !strings.Contains(out, "FAIL visibility: public repo: every PR runs CI, drop the check job's if:") {
		t.Errorf("public with if:\n%s", out)
	}
}

func TestRun_whenKitTagsAbsentOrUnreachable_thenNoteOrFail(t *testing.T) {
	r := app(t)
	r.opts.KitTags = func() ([]string, error) { return []string{"v1.0.0"}, nil }
	if out, err := r.run(appShip); err != nil || !strings.Contains(out, "ok gomod: no kit/v* tag yet\n") {
		t.Errorf("no tags: %v\n%s", err, out)
	}
	r.opts.KitTags = func() ([]string, error) { return nil, errors.New("ls-remote: offline") }
	if out, err := r.run(appShip); err == nil || !strings.Contains(out, "FAIL gomod: ls-remote: offline (backend/go.mod)") {
		t.Errorf("offline: %v\n%s", err, out)
	}
	// A service without kit: nothing to require before kit's first tag.
	r.edit("backend/go.mod", "github.com/trick77/ship/kit", "example.com/other")
	calls := 0
	r.opts.KitTags = func() ([]string, error) { calls++; return nil, nil }
	if out, err := r.run(appShip); err != nil || !strings.Contains(out, "ok kit: no kit/v* tag yet\n") {
		t.Errorf("no tags, no kit: %v\n%s", err, out)
	}
	r.opts.KitTags = func() ([]string, error) { calls++; return nil, errors.New("ls-remote: offline") }
	if out, err := r.run(appShip); err == nil || !strings.Contains(out, "FAIL kit: ls-remote: offline (backend/go.mod)") {
		t.Errorf("offline, no kit: %v\n%s", err, out)
	}
	if calls != 2 {
		t.Errorf("%d ls-remotes, want one per run", calls)
	}
}

func TestVisibility_whenAPI_thenAnsweredAndCached(t *testing.T) {
	status := http.StatusNotFound
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		if req.URL.Path != "/repos/o/app" {
			t.Errorf("path %s", req.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	r := newRepo(t, nil)
	now := time.Now()
	r.opts = Options{Out: &r.out, API: srv.URL, Getenv: fakeEnv(nil), Now: func() time.Time { return now }}.withDefaults()
	e := &env{root: r.dir, c: r.config(""), o: r.opts, owner: "o", repo: "app"}
	if v, err := e.visibility(); v != "private" || err != nil {
		t.Fatalf("404: %q %v", v, err)
	}
	status = http.StatusOK
	if v, err := e.visibility(); v != "private" || err != nil || calls != 1 {
		t.Fatalf("cached: %q %v, %d calls", v, err, calls)
	}
	now = now.Add(25 * time.Hour)
	if v, err := e.visibility(); v != "public" || err != nil || calls != 2 {
		t.Fatalf("expired: %q %v, %d calls", v, err, calls)
	}
	now = now.Add(25 * time.Hour)
	status = http.StatusForbidden
	if _, err := e.visibility(); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("403: %v", err)
	}
	srv.Close()
	if _, err := e.visibility(); err == nil || !strings.Contains(err.Error(), "no cached answer") {
		t.Fatalf("offline: %v", err)
	}
	e.owner = ""
	if _, err := e.visibility(); err == nil || !strings.Contains(err.Error(), "no GitHub origin") {
		t.Fatalf("no origin: %v", err)
	}
}

func TestRun_whenVisibilityUnknown_thenScaffoldAndVisibilityFail(t *testing.T) {
	r := app(t)
	r.opts.Getenv = fakeEnv(map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_EVENT_PATH": "/nonexistent"})
	r.opts.API = "http://127.0.0.1:1"
	out, err := r.run(appShip)
	if err == nil || !strings.Contains(out, "FAIL visibility: cannot ask GitHub") ||
		!strings.Contains(out, "FAIL scaffold: repo visibility unknown") {
		t.Errorf("%v\n%s", err, out)
	}
	// ship.yaml's visibility stands in for the failed lookup: a note, no FAIL.
	out, err = r.run("visibility: private\n" + appShip)
	if err != nil || !strings.Contains(out, "ok visibility: GitHub lookup failed (cannot ask GitHub") ||
		!strings.Contains(out, "ship.yaml's visibility: private used") {
		t.Errorf("with visibility: %v\n%s", err, out)
	}
	if err := Init(r.dir, r.config(appShip), r.opts, false); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("init: %v", err)
	}
}

func TestRun_whenSetupHasBlockComment_thenSkipped(t *testing.T) {
	r := app(t)
	r.write("ui/src/test/setup.ts", "/**\n * License header.\n */\n// and a line\n/* one */ "+appSetup)
	if out, err := r.run(appShip); err != nil || !strings.Contains(out, "ok ui\n") {
		t.Errorf("block comment: %v\n%s", err, out)
	}
	// The import inside the comment is no statement.
	r.write("ui/src/test/setup.ts", "/*\nimport \"@testing-library/jest-dom/vitest\";\n*/\nimport { vi } from \"vitest\";\n")
	if out, _ := r.run(appShip); !strings.Contains(out, "FAIL ui: first statement is not") {
		t.Errorf("commented import:\n%s", out)
	}
}

func TestRun_whenRateLimited_thenDeclaredVisibilityStands(t *testing.T) {
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	r := app(t)
	r.opts.Getenv, r.opts.API = fakeEnv(nil), srv.URL
	out, err := r.run("visibility: private\n" + appShip)
	if err != nil || !strings.Contains(out, "ok visibility: GitHub lookup failed (GitHub API") {
		t.Errorf("403: %v\n%s", err, out)
	}
	// A lookup that answers and contradicts ship.yaml still fails.
	status = http.StatusOK
	out, err = r.run("visibility: private\n" + appShip)
	if err == nil || !strings.Contains(out, "FAIL visibility: visibility: private, GitHub says public") {
		t.Errorf("200: %v\n%s", err, out)
	}
}

func TestInit_whenDiff_thenNothingWritten(t *testing.T) {
	r := app(t)
	r.edit(scaffold.Release, "default: patch", "default: minor")
	r.out.Reset()
	err := Init(r.dir, r.config(appShip), r.opts, true)
	if err == nil || !strings.Contains(r.out.String(), "-        default: minor\n+        default: patch\n") {
		t.Fatalf("%v\n%s", err, r.out.String())
	}
	r.out.Reset()
	if err := Fix(r.dir, r.config(appShip), r.opts); err != nil {
		t.Fatalf("fix: %v\n%s", err, r.out.String())
	}
	if !strings.Contains(r.out.String(), "ship: wrote .github/workflows/release.yaml\n") {
		t.Errorf("fix:\n%s", r.out.String())
	}
	r.out.Reset()
	if err := Init(r.dir, r.config(appShip), r.opts, true); err != nil || r.out.Len() != 0 {
		t.Errorf("after fix: %v\n%s", err, r.out.String())
	}
}

func TestFix_whenManualLeft_thenListed(t *testing.T) {
	r := app(t)
	r.remove(scaffold.Dependabot)
	r.write("Makefile", "check:\n\tgo test ./...\n")
	r.write("AGENTS.md", "")
	r.out.Reset()
	err := Fix(r.dir, r.config(appShip), r.opts)
	out := r.out.String()
	if err == nil || !strings.Contains(out, "ship: wrote .github/dependabot.yaml\n") ||
		!strings.Contains(out, "FAIL scaffold: Makefile has its own check") || !strings.Contains(out, "FAIL agents:") ||
		strings.Contains(out, "FAIL dependabot") {
		t.Errorf("%v\n%s", err, out)
	}
}

func TestParseOrigin(t *testing.T) {
	for u, want := range map[string]string{
		"https://github.com/trick77/ship.git\n": "trick77/ship",
		"git@github.com:ipverse/core.git":       "ipverse/core",
		"ssh://git@github.com/o/r":              "o/r",
		"https://gitlab.com/o/r.git":            "/",
		"https://github.com/o":                  "/",
		"https://github.com/o/r/x":              "/",
	} {
		if o, r := parseOrigin(u); o+"/"+r != want {
			t.Errorf("%q: %s/%s", u, o, r)
		}
	}
}

func TestAtLeast(t *testing.T) {
	for _, tc := range []struct {
		v, floor string
		want     bool
	}{{"1.27.0", "1.27", true}, {"1.27.9", "1.27", true}, {"1.28.0", "1.27", true}, {"2.0.0", "1.27", true},
		{"1.26.9", "1.27", false}, {"26", "26", true}, {"27", "26", true}, {"24", "26", false}} {
		if got := atLeast(tc.v, tc.floor); got != tc.want {
			t.Errorf("atLeast(%s, %s) = %v", tc.v, tc.floor, got)
		}
	}
}

func TestKitTags_whenOffline_thenError(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := kitTags(); err == nil {
		t.Error("no git, no error")
	}
}
