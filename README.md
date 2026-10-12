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
  sha="$(git ls-remote https://github.com/trick77/ship refs/heads/master | cut -f1)"
  [ -n "$sha" ] || { echo "ship: cannot resolve ship's master on GitHub (git ls-remote, error above)" >&2; return 1; }
  go run "github.com/trick77/ship@$sha" "$@"
}
ship hooks   # once per clone: the pre-push hook (runs master's ship on every push)
ship check   # the gate by hand
```

`ship.yaml` at the repo root (every key optional):

```yaml
base: master          # patch gate diffs against origin/<base>; release requires HEAD = origin/<base>
lint: v2.14.0         # golangci-lint, built by go run; leave unset: ship's version, the same everywhere
postgres: "17"        # go test gets postgres:17-alpine, its DSN in SHIP_TEST_DSN; omit for none
shell: [hooks/pre-commit]   # shellcheck these too (*.sh and sh/bash shebangs are found by themselves)
patch: 75             # min coverage of changed lines, %
tag_prefix: ""        # "v" for a Go module
go:
  dir: .              # module dir, e.g. backend
  floor: 75           # min line coverage, %
  race: true
  coverpkg: ./internal/...
  exclude: [cmd/]     # not counted by either coverage gate
ui:                   # omit for no UI
  dir: ui
  floor: 75
  build: npm ci && npm run build      # tsc -b && vite build: the typecheck
  lint: npm run lint                  # the default: eslint / prettier --check; "" = none
  test: npm run test -- --coverage --coverage.reporter=lcov --coverage.reporter=text-summary  # must write lcov
  lcov: ui/coverage/lcov.info         # repo-relative; SF: paths relative to ui.dir
  embed: backend/web/dist             # go:embed dir, emptied but for .gitkeep before ui.build; omit for none
steps:                # repo-specific checks, run after lint
  - { name: openapi lint, run: npx spectral lint api.yaml }
images:               # release builds these; none = tag + GitHub release only
  - { suffix: "", target: "", containerfile: Containerfile, platforms: [linux/amd64] }
```

A platform other than amd64 builds under QEMU on the amd64 runner: every `RUN` in its stages is emulated, several
times slower, inside the release job's 30 minutes. Cross-compile Go on the build platform
(`FROM --platform=$BUILDPLATFORM golang…`, `GOOS=$TARGETOS GOARCH=$TARGETARCH`) so only the runtime stage is emulated.

The Go coverage report lives in the git dir (`.git/ship/`), never in the tree. Gitignore the UI test runner's
coverage output.

## Commands

| | |
|---|---|
| `ship check` | fetch, commit subjects, secret scan, prettier pin, ui embed, ui build, gofmt, go mod verify, go mod tidy, build, govulncheck, golangci-lint (incl. govet), ui lint, shellcheck, steps, postgres, go test (-race, coverage), ui test, floors, patch gate |
| `ship lint` | golangci-lint alone, at the version `ship check` runs: a Makefile's quick lint, no version of its own |
| `ship hooks` | install the pre-push hook (respects core.hooksPath, never overwrites a foreign hook) |
| `ship release [patch\|minor\|major\|auto] [-cache gha] [-dry-run]` | next version from tags, notes from the subjects since the last tag (grouped by area), images pushed (version + sha), tag (API), GitHub release, then :MAJOR.MINOR, :MAJOR, :latest. No `images:` and no `ui:`: `go build ./...` before the tag (in `-dry-run` too). With `images:` only on a GitHub-hosted runner (the Release workflow); elsewhere `-dry-run` only. The Release workflow inherits the caller's permissions: `packages: write` only with `images:` |

Gates beyond the build and tests, all hard failures:

- golangci-lint: the family config embedded in ship (`internal/check/golangci.yaml`), unless the repo keeps its own
  `.golangci.{yml,yaml,toml,json}` between `go.dir` and the root.
- govulncheck (pinned, `go run`): a known vulnerability the module's code reaches.
- `go mod tidy -diff`: go.mod/go.sum as tidy leaves them.
- Secret scan: the tree and every line and message the commits not yet on origin/<base> add, for API key shapes
  (`sk-`, `tp-`, GitHub tokens, AWS key ids) and `.env` files. A fixture line carrying `ship:allow-secret` passes.
- shellcheck -x on `*.sh`, sh/bash shebang files and `shell:`; needs `shellcheck` on PATH (GitHub's ubuntu runners
  have it).
- Prettier pin: with a UI, `devDependencies.prettier` in `<ui.dir>/package.json` is exactly ship's pin (3.9.9).
- `postgres:`: `SHIP_TEST_DSN` set already (a server of one's own) is used as is; else check starts the container
  (docker, loopback, free port), waits for it and removes it after go test, on failure and on Ctrl-C too.

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

Patch gate: changed lines (committed, staged, unstaged, untracked) against the merge-base with origin/<base>,
intersected with the coverage reports; comment and blank lines never count. `[skip patch-coverage]` in a commit
message skips it (floors still apply). A report path that names no file of the repo, exactly spelled, fails the
gate (paths off: wrong `go.dir`, `ui.dir`, `ui.lcov` or vitest root); with a report broken or missing, the patch
gate does not run rather than pass over the other side alone. Scope is the reports': Go packages outside
`go.coverpkg` and UI files outside vitest's `coverage.include` are not measured.

Pre-push: deletes and tags pass; anything else must be HEAD with a clean tree (check tests the tree), then
`ship check`. A tree with no `ship.yaml` in the index (a branch cut before ship) is refused: rebase it.

## Workflows

Reusable, for running the same commands on GitHub:

```yaml
jobs:
  check:
    uses: trick77/ship/.github/workflows/check.yaml@master      # inputs: go-dir, node-version, ui-dir, postgres
  release:
    uses: trick77/ship/.github/workflows/release.yaml@master    # inputs: bump, go-dir
  cleanup:
    uses: trick77/ship/.github/workflows/cleanup-images.yaml@master   # inputs: packages (JSON), keep, dry-run
```

## Adopting ship

A repo moving to ship, in one PR (its own subject already Go style: the new gate checks it):

1. `ship.yaml` at the root: only the keys that differ from the defaults above.
2. `ship hooks` in every clone; drop the repo's own gate scripts and hooks.
3. `.github/workflows/ci.yaml` and `release.yaml` calling the reusable ones: copy the caller from the head comment
   of `check.yaml` / `release.yaml`. `release.yaml`: `workflow_dispatch` only. `ci.yaml`: `pull_request` +
   `workflow_dispatch`, since Dependabot and fork PRs never pass the hook. Public repo: every PR, forks included
   (read-only token, no secrets). Private repo: Dependabot PRs only (the caller's `if:`), Actions minutes are billed
   and own pushes are gated by the hook already.
4. `ship check` green locally, then push: the hook runs it.
5. AGENTS.md, verbatim with the repo's own areas:
   ``- Commit subjects + PR titles: Go style `area: what` (`<area>: …`, `<area>: …`), gated by ship; release notes list them by area.``
6. `.github/dependabot.yaml`: `commit-message: { prefix: deps }` on every ecosystem (`deps: bump …`). The gate
   skips bots either way; the prefix keeps them out of "other" in the release notes.
