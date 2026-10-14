# ship

The gate, pre-push hook and release for the trick77 and ipverse Go repos, as one versioned tool instead of
scripts copied into every repo.

GitHub must never be first to find a failure. `ship check` runs everything CI checks, locally; the pre-push hook
runs it before every push; CI runs the same command on pull requests (Dependabot, outside contributors).

## Use in a repo

Unpinned: every run is master's ship (no pin to bump; a broken master commit reaches every repo at once, so merging
to master only what is releasable is the only guard). A proxy may lag on `@master`: resolve master's sha on GitHub,
as the hook does, and run that.

```sh
ship() {
  local sha
  sha="$(git ls-remote https://github.com/trick77/ship refs/heads/master | awk '$2 == "refs/heads/master" { print $1 }')"
  printf '%s\n' "$sha" | grep -Eqx '[0-9a-f]{40}' ||
    { echo "ship: cannot resolve ship's master on GitHub: '$sha' (git ls-remote, error above)" >&2; return 1; }
  go run "github.com/trick77/ship@$sha" "$@"
}
ship hooks   # once per clone: the pre-push hook (runs master's ship on every push)
ship check   # the gate by hand
```

`ship.yaml` at the repo root (every key optional):

```yaml
base: master          # patch gate diffs against origin/<base>; release requires HEAD = origin/<base>
lint: v2.14.0         # golangci-lint, built by go run; leave unset: ship's version, the same everywhere
postgres: "17"        # go test gets postgres:17-alpine (or a tagged image: timescale/timescaledb:2.29.1-pg17, or compose:db: that root compose service's image), DSN in SHIP_TEST_DSN
shell: [hooks/pre-commit]   # shellcheck these too (*.sh and sh/bash shebangs are found by themselves)
patch: 75             # min coverage of changed lines, %
tag_prefix: ""        # "v" for a Go module
go:
  dir: .              # module dir, e.g. backend
  floor: 75           # min line coverage, %
  race: true
  coverpkg: ./...     # go test -coverpkg (cross-package attribution); e.g. ./internal/... to narrow, "" or null = none
  exclude: [cmd/]     # not counted by either coverage gate
  modules: [kit, kit/auth]   # nested modules (own go.mod), gated like go.dir; see below
ui:                   # omit for no UI
  dir: ui
  floor: 75
  build: npm ci && npm run build      # tsc -b && vite build: the typecheck
  lint: npm run lint                  # eslint / prettier --check, the only value conform passes; "" = none, needs a ui allow
  test: npm run test -- --coverage --coverage.reporter=lcov --coverage.reporter=text-summary  # must write lcov
  lcov: coverage/ui/lcov.info         # repo-relative, vite reportsDirectory "../coverage/ui"; SF: paths relative to ui.dir
  embed: backend/web/dist             # go:embed dir, emptied but for .gitkeep before ui.build; omit for none
steps:                # repo-specific checks, run after lint
  - { name: openapi lint, run: npx spectral lint api.yaml }
images:               # release builds these; none = tag + GitHub release only
  - { suffix: "", target: "", containerfile: Containerfile, platforms: [linux/amd64] }
visibility: private   # conform checks it against GitHub's; unset = GitHub's
allow:                # conform exceptions, why required (see Conform)
  - { rule: distroless, image: -agent, why: smartctl needs root }
dependabot:           # ignores for ship's dependabot.yaml: under every entry of the ecosystem, why as a comment;
  ignore:             # an ecosystem the file has no entry for fails init and conform
    - { ecosystem: npm, dependency: mermaid, versions: [">=12.1.0"], why: drops a diagram type }
    - { ecosystem: gomod, dependency: example.com/pair, why: bumped by hand with its twin }   # no versions: all
```

A platform other than amd64 builds under QEMU on the amd64 runner: every `RUN` in its stages is emulated, several
times slower, inside the release job's 30 minutes. Cross-compile Go on the build platform
(`FROM --platform=$BUILDPLATFORM golang…`, `GOOS=$TARGETOS GOARCH=$TARGETARCH`) so only the runtime stage is emulated.

`go.modules`: every nested Go module (a go.mod below the repo other than go.dir's, outside `testdata/` and
`vendor/`) is listed, or check fails: `go ./...` stops at a nested go.mod, so nothing else would build, lint or test
it. Each listed module gets go.dir's gates (mod verify, tidy, build, govulncheck, golangci-lint, go test with
`go.race`), its own floor at `go.floor` (a module is released and imported on its own: the root's lines never carry
it), and its changed lines in the patch gate; `go.exclude` applies, `go.coverpkg` is go.dir's alone. Entries are
cleaned like go.dir (`./kit/auth/` is `kit/auth`); one outside the repo (absolute or through `..`), the root or a
duplicate fails.

The Go coverage report lives in the git dir (`.git/ship/`), never in the tree. The UI test runner's (`ui.lcov`'s
dir) is in `.gitignore`'s ship block.

## Commands

| | |
|---|---|
| `ship check` | conform, fetch, commit subjects, secret scan, ui embed, ui build, npm audit, go modules, gofmt, go mod verify, go mod tidy, build, govulncheck, golangci-lint (incl. govet), ui lint, shellcheck, steps, postgres, go test (-race, coverage), ui test, floors, patch gate |
| `ship conform [-fix]` / `ship init [-diff]` | the repo against ship's canon / write the files ship owns (see Conform) |
| `ship lint` | golangci-lint alone, at the version `ship check` runs: a Makefile's quick lint, no version of its own |
| `ship hooks` | install the pre-push hook (respects core.hooksPath, never overwrites a foreign or symlinked hook) |
| `ship release [patch\|minor\|major\|auto] [-cache gha] [-dry-run] [-dir <module>]` | next version from tags, notes from the subjects since the last tag (grouped by area), tag (API), images pushed (version + sha), scanned and signed, GitHub release, then :MAJOR.MINOR, :MAJOR, :latest. No `images:` and no `ui:`: `go build ./...` before the tag (in `-dry-run` too). With `images:` only on a GitHub-hosted runner (the Release workflow); elsewhere `-dry-run` only. The Release workflow inherits the caller's permissions: `packages: write` and `id-token: write` only with `images:`. `-dir`: a nested Go module, below |

Gates beyond the build and tests, all hard failures:

- golangci-lint: the family config embedded in ship (`internal/check/golangci.yaml`), unless the repo keeps its own
  `.golangci.{yml,yaml,toml,json}` between `go.dir` and the root.
- govulncheck (pinned, `go run`): a known vulnerability the module's code reaches.
- npm audit (with `ui:`, in `ui.dir`, after ui build): `npm audit --audit-level=high --omit=dev`, a high or
  critical advisory in what the UI ships; dev dependencies only build it.
- `go mod tidy -diff`: go.mod/go.sum as tidy leaves them.
- Secret scan: the tree and every line and message the commits not yet on origin/<base> add, for API key shapes
  (`sk-`, `tp-`, GitHub tokens, AWS key ids) and `.env` files. A fixture line carrying `ship:allow-secret` passes.
- shellcheck -x on `*.sh`, sh/bash shebang files and `shell:`; needs `shellcheck` on PATH (GitHub's ubuntu runners
  have it).
- `postgres:`: `SHIP_TEST_DSN` set already (a server of one's own) is used as is; else check starts the container
  (docker, loopback, free port), waits for it and removes it after go test, on failure and on Ctrl-C too.
  `compose:<service>`: the image that service names in the root compose files (`compose*.yaml`,
  `docker-compose*.yaml`, `.yml` too), the one pin Dependabot's docker-compose ecosystem bumps; a missing service, no `image:`,
  `${VAR}`, no tag or two files disagreeing fail. A literal image a compose service names too fails conform.

TypeScript/React: `ui.build` is the typecheck (`tsc -b && vite build`), `ui.lint` runs ESLint/Prettier, `ui.test`
runs vitest with coverage; its lcov feeds the UI floor and the patch gate for changed `.ts`/`.tsx` lines.
Playwright e2e goes in `steps`. ship's CI proves this on `testdata/ui`, a real React + TypeScript + vite +
vitest project (npm ci, typecheck, build, coverage, an untested `.tsx` change failing the patch gate).

Commit subjects: Go style (go.dev/doc/contribute), `area: what`, e.g. `hook: pin GIT_DIR before cd` or
`net/http, net/url: fix parsing`. No trailing period, at most 76 characters (a squash merge's ` (#N)` not
counted). Every non-merge commit on origin/<base>..HEAD is checked, `[bot]` authors (Dependabot) skipped,
`Revert "…"` and `fixup!` commits judged by what they wrap. The Check workflow adds a pull request's title
(`SHIP_PR_TITLE`, not a bot's): a squash merge makes it master's subject. A PR from a fork (`SHIP_PR_FORK=true`): the title
alone, an outside contributor's branch subjects never reach master. Release notes group by the first area.

`release auto`: patch, unless a commit message since the last tag has a line `Release: minor` or `Release: major`
(highest wins; a minor or major also needs a `Release-note: …` line, which goes above the notes, a bump by hand
too). The gate checks these lines on the branch: a typo fails the push, never a release after the merge. Nothing but
`*.md`, `LICENSE`, `*_test.go` and `testdata/` changed: no release, exit 0. HEAD behind origin/<base>: skipped, the
newer push's run releases both. No previous tag: refused, the first one is cut by hand. ship releases itself this
way from CI on every master push.

Images: OCI labels (`org.opencontainers.image.version`, `.revision` = HEAD, `.source`, `.created` = HEAD's commit
time, so a rebuild labels the same), an SBOM and `mode=max` provenance as attestations. After the push, trivy (pinned,
`internal/pins`) scans each image by digest: a HIGH or CRITICAL with a fix available fails the release and gives the
tag back (the pushed version + sha tags stay until the rerun overwrites them; the moving ones never move). Then
cosign signs each digest keyless, with the workflow's OIDC token: the caller grants `id-token: write` along with
`packages: write`. Verify: `cosign verify --certificate-identity-regexp 'https://github.com/<owner>/<repo>/' --certificate-oidc-issuer https://token.actions.githubusercontent.com ghcr.io/<owner>/<repo>:<version>`.

Nested Go modules (`-dir kit`; the Release workflow's `dir` input): tags `<dir>/vX.Y.Z` (`tag_prefix: v` required),
`go build` in the dir, notes and `Release:` lines from the commits touching it, `release auto` counts only its
changes, never those of a module nested below it (kit's release ignores `kit/auth/`); the root's release ignores
its nested modules too, unless it has `images:` (a build context may copy them). No images, not the repo's
"Latest" release. No go.mod in the dir: auto skips (CI can list a module before it merges). No previous tag: auto
cuts 0.1.0, nothing to confuse a nested module's prefix with. ship's CI releases `kit` and `kit/auth` this way;
`go.modules` gates them in check.

Patch gate: changed lines (committed, staged, unstaged, untracked) against the merge-base with origin/<base>,
intersected with the coverage reports; comment and blank lines never count. `[skip patch-coverage]` in a commit
message skips it (floors still apply). A report path that names no file of the repo, exactly spelled, fails the
gate (paths off: wrong `go.dir`, `ui.dir`, `ui.lcov` or vitest root); with a report broken or missing, the patch
gate does not run rather than pass over the other side alone. Scope is the reports': Go packages outside
`go.coverpkg` and UI files outside vitest's `coverage.include` are not measured. The default `./...` instruments
every package for every test binary, the slower run; `./internal/...` or `""` narrows it.

Pre-push: deletes and tags pass; anything else must be HEAD with a clean tree (check tests the tree), then
`ship check`. A tree with no `ship.yaml` in the index (a branch cut before ship) is refused: rebase it. A hook
ship installed that is not this ship's script is rewritten with it (renamed over, so the running hook is
unharmed; the old one kept as `pre-push.ship-old`), one line says so; a foreign or symlinked hook is never touched. Under
`SHIP_CMD` (not master's ship) a stale hook only gets a warning.

## Workflows

Reusable, for running the same commands on GitHub:

```yaml
jobs:
  check:
    uses: trick77/ship/.github/workflows/check.yaml@master      # inputs: go-dir, node-version, ui-dir, postgres (deprecated: ship.yaml postgres:)
  release:
    uses: trick77/ship/.github/workflows/release.yaml@master    # inputs: bump, go-dir, dir
  cleanup:
    uses: trick77/ship/.github/workflows/cleanup-images.yaml@master   # inputs: packages (JSON), keep, dry-run
```

## Conform

`ship conform` checks the repo against ship's canon, first in every `ship check` (so on every push): every
divergence the consumer audit found was a copied file that drifted or an adoption step forgotten. Hard fail, no warn
mode. One line per rule: `ok`, `FAIL <rule>: <what> (<file>)`, `allowed <rule>: <why>`, `skip <rule>: <why>` (a
library without images or UI skips what does not apply). `ship conform -fix` runs `ship init`, then conforms:
what is left is by hand.

`ship init` writes the files ship owns, rendered from `ship.yaml` and the repo (origin, visibility, Containerfiles):
`.github/workflows/ci.yaml`, `release.yaml`, `cleanup-images.yaml` (images), `.github/dependabot.yaml`,
`<ui.dir>/eslint.config.js`, `tsconfig.node.json` and `.npmrc` (UI), and in four files only a block, the rest of
each is the repo's. Owned means byte-equal.

| file | block | where | holds |
|---|---|---|---|
| `Makefile` | `# ship:begin` … `# ship:end` | last (the default goal stays the repo's) | `check`, `hooks`, `lint` on master's ship; apps, or one that exists. Its own `check:`/`hooks:`/`lint:`/`SHIP_CMD` outside the block: moved in by hand |
| `.dockerignore` | `# ship:begin` … `# ship:end` | first (later lines win) | the build context; images |
| `.gitignore` | `# ship:begin` … `# ship:end` | last | `/.playwright-mcp/`, the UI coverage dir; always |
| `AGENTS.md` | `<!-- ship:begin -->` … `<!-- ship:end -->` | last | gate (`make check`, or `ship check` without a Makefile), hooks, CI, owned files, kit (images), release command, commit subjects; always |

A missing file is created holding only its block; a file without one gets it appended (prepended), an existing
block is replaced in place.
The CI caller's `node-version` is the Containerfile's `node:N` builder major (26 without one): Dependabot moves the
builder, `ship init` moves CI after it.
`ship init -diff` prints the difference, writes nothing, exits 1 when anything differs.

| rule | what | allowable |
|---|---|---|
| `scaffold` | every owned file equals ship's template. The rules below read the owned files too; on one `scaffold` fails, not allowed, their findings are dropped (`ship init` rewrites it); an allowed diff keeps them | per `path` |
| `workflows` | ci/release/cleanup callers use `trick77/ship/.github/workflows/<x>.yaml@master`; release: `packages: write` and `id-token: write` (cosign) ⇔ images, bump options include `auto`; no other image cleanup action, in any workflow | yes |
| `visibility` | GitHub's visibility (Actions event, else the API unauthenticated, cached a day in the git dir) matches `visibility:`; a failed lookup: `visibility:` stands (a note), unset = FAIL; public: CI on every PR (no `if:`); private: Dependabot-only `if:`, every npm/docker/docker-compose update grouped, ≤ 5 open PRs, cleanup caller with images | yes |
| `dependabot` | `commit-message: { prefix: deps }` everywhere; gomod (go.dir, go.modules), github-actions, npm (UI), docker (Containerfile), docker-compose `/` (a root `compose*.yaml` naming an image by a version tag) present | yes |
| `containerfile` | per image: `ARG VERSION` into `-X github.com/trick77/ship/kit/version.Version=${VERSION}`, `-trimpath`, non-root final stage, builder `golang:X.Y` = go.mod's and ≥ ship's Go floor (1.27), builder `node:N` ≥ ship's Node floor (26), every base (not a stage, `scratch` or `${ARG}`) with a tag or digest (untagged = a floating `:latest` Dependabot cannot track) | per `image` |
| `distroless` | final stage on `gcr.io/distroless/` | per `image` |
| `dockerignore` | exists with images; excludes `<ui.dir>/node_modules` and `<ui.dir>/dist` | yes |
| `ui` | `ui.lint: npm run lint`; scripts exactly `build` = `tsc -b && vite build`, `lint` = `eslint . --max-warnings 0 && npm run format:check`, `format` = `prettier --write 'src/**/*.{ts,tsx,css}' '*.ts'`, `format:check` = `prettier --check …` (same globs), `test` = `vitest run`; devDependencies `typescript`, `prettier`, `eslint`, `typescript-eslint`, `eslint-config-prettier`, `@eslint/js`, `eslint-plugin-react-hooks` present at an exact version (no range; the version is the repo's), typescript below `pins.TypescriptCap` (typescript-eslint); eslint.config.js; tsconfig.json references `./tsconfig.node.json`; vite.config.ts: `defineConfig` from `vitest/config`, `globals: true`, `setupFiles: ["./src/test/setup.ts"]`, `reportsDirectory: "../coverage/ui"`; `src/test/setup.ts`'s first statement `import "@testing-library/jest-dom/vitest";` | yes |
| `golangci` | no `.golangci.yaml`/`.yml` in the repo: ship's config is the family's | per `path` |
| `gomod` | `go x.y.z` ≥ 1.27.0 in go.dir and every go.modules module; kit's version is Dependabot's to move | yes |
| `kit` | a repo with images requires kit | yes |
| `handrolled` | no `internal/version` package, `/health`, `/healthz` or `/readyz` route (a method pattern such as `"GET /healthz"`, or a `Handle`/`HandleFunc` argument; a URL such as `LocalURL(addr, "/healthz")` is none; another router's methods and a path held in a constant are not seen), `signal.Notify`/`NotifyContext`, `github.com/coreos/go-oidc` import outside tests: kit (kit/auth) has them | per `path` |
| `agents` | AGENTS.md has the commit-subject line (its ship block carries it; the backstop for an allowed `scaffold` diff) | yes |
| `postgres` | `compose:<service>` resolves; a literal `postgres:` (a version too) names no repository a root compose service's `image:` names (`duplicates compose service <s>'s image, use compose:<s>`): two pins drift | yes |

An exception lives in `ship.yaml`, its why required (an entry without one fails the parse):

```yaml
visibility: private   # optional: checked against GitHub's
allow:
  - { rule: distroless, image: -agent, why: smartctl's passthrough ioctls need root and busybox }
  - { rule: scaffold, path: ui/eslint.config.js, why: the thin-space number rule }
```

## Adopting ship

A repo moving to ship, in one PR (its own subject already Go style: the new gate checks it):

1. `ship.yaml` at the root: only the keys that differ from the defaults above.
2. `ship init`: the workflow callers, dependabot, the `Makefile`, `.dockerignore`, `.gitignore` and `AGENTS.md`
   blocks, the UI's ESLint config, tsconfig.node.json and .npmrc. Drop the repo's own gate scripts and hooks, and
   the AGENTS.md lines the block now says (gate, hooks, release, commit subjects).
3. `ship conform`: what init cannot write (Containerfile, package.json scripts and devDependencies, vite config, kit)
   by hand, or an `allow` entry with its why.
4. `ship hooks` in every clone.
5. `ship check` green locally, then push: the hook runs it.
