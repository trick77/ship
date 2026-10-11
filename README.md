# ship

The gate, pre-push hook and release for the trick77 and ipverse Go repos, as one versioned tool instead of
scripts copied into every repo.

GitHub must never be first to find a failure. `ship check` runs everything CI checks, locally; the pre-push hook
runs it before every push; CI (by hand) runs the same command.

## Use in a repo

```sh
go get -tool github.com/trick77/ship@latest   # pins it in go.mod; Dependabot (gomod) bumps it
go tool ship hooks                            # once per clone: the pre-push hook
```

`ship.yaml` at the repo root (every key optional):

```yaml
base: master          # patch gate diffs against origin/<base>; release requires HEAD = origin/<base>
lint: v2.14.0         # golangci-lint, built by go run: same version everywhere
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
  lint: npm run lint                  # eslint / prettier --check; empty = none
  test: npm run test -- --coverage    # must write lcov
  lcov: ui/coverage/lcov.info         # repo-relative; SF: paths relative to ui.dir
steps:                # repo-specific checks, run after lint
  - { name: openapi lint, run: npx spectral lint api.yaml }
images:               # release builds these; none = tag + GitHub release only
  - { suffix: "", target: "", containerfile: Containerfile, platforms: [linux/amd64] }
```

The Go coverage report lives in the git dir (`.git/ship/`), never in the tree. Gitignore the UI test runner's
coverage output.

## Commands

| | |
|---|---|
| `go tool ship check` | fetch, ui build, gofmt, build, golangci-lint (incl. govet), ui lint, steps, go test (-race, coverage), ui test, floors, patch gate |
| `go tool ship hooks` | install the pre-push hook (respects core.hooksPath, never overwrites a foreign hook) |
| `go tool ship release [patch\|minor\|major] [-cache gha] [-dry-run]` | next version from tags, images pushed (version + sha), tag (API), GitHub release, then :MAJOR.MINOR, :MAJOR, :latest |

TypeScript/React: `ui.build` is the typecheck (`tsc -b && vite build`), `ui.lint` runs ESLint/Prettier, `ui.test`
runs vitest with coverage; its lcov feeds the UI floor and the patch gate for changed `.ts`/`.tsx` lines.
Playwright e2e goes in `steps`. ship's CI proves this on `testdata/ui`, a real React + TypeScript + vite +
vitest project (npm ci, typecheck, build, coverage, an untested `.tsx` change failing the patch gate).

Patch gate: changed lines (committed, staged, unstaged, untracked) against the merge-base with origin/<base>,
intersected with the coverage reports; comment and blank lines never count. `[skip patch-coverage]` in a commit
message skips it (floors still apply).

Pre-push: deletes and tags pass; anything else must be HEAD with a clean tree (check tests the tree), then
`ship check`. A branch whose go.mod has no `tool github.com/trick77/ship` is refused: rebase it.

## Workflows

Reusable, for running the same commands on GitHub by hand:

```yaml
jobs:
  check:
    uses: trick77/ship/.github/workflows/check.yaml@v0.1.0      # inputs: go-dir, node-version, ui-dir
  release:
    uses: trick77/ship/.github/workflows/release.yaml@v0.1.0    # inputs: bump, go-dir
  cleanup:
    uses: trick77/ship/.github/workflows/cleanup-images.yaml@v0.1.0   # inputs: packages (JSON), keep, dry-run
```
