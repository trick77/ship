[[- $run := "ship" ]][[ if .MakefileOwned ]][[ $run = "make" ]][[ end -]]
<!-- ship:begin -->
<!-- Owned by ship init (github.com/trick77/ship), never edited by hand: repo-specific lines go outside the markers. -->
## ship

[[ if .MakefileOwned -]]
- Gate: `make check` before every push, green. `make hooks` once per clone and again when ship says the hook is
  stale: the pre-push hook runs the gate. GitHub is never the first to find a failure.
[[ else -]]
- Gate: `ship check` (the `ship()` function in github.com/trick77/ship's README) before every push, green.
  `ship hooks` once per clone and again when ship says the hook is stale: the pre-push hook runs the gate. GitHub is
  never the first to find a failure.
[[ end -]]
[[ if .Private -]]
- CI: Dependabot PRs only (Actions minutes). A Dependabot PR: `[[ $run ]] check` on its branch before merging it.
[[ else -]]
- CI: every PR (Dependabot and outside contributors have no pre-push hook).
[[ end -]]
- Owned by ship, changed with `ship init`, never by hand: the `.github/workflows/` callers, `.github/dependabot.yaml`
  and the ship blocks in [[ if .MakefileOwned ]]Makefile, [[ end ]][[ if .Images ]].dockerignore, [[ end ]].gitignore and AGENTS.md.
[[- if and .UI (eq .UIDir ".") ]]
  At the repo root: eslint.config.js, tsconfig.node.json and .npmrc.
[[- else if .UI ]]
  In `[[ .UIDir ]]/`: eslint.config.js, tsconfig.node.json and .npmrc.
[[- end ]]
  No `.golangci.yaml`: ship's config is the family's.
[[ if .Images -]]
- kit (github.com/trick77/ship/kit): version, `/healthz` `/readyz`, signals and graceful shutdown come from kit,
  never hand-rolled. Dependabot bumps it in its own group: a v0.x minor may break.
[[ end -]]
- Release: by hand only, `gh workflow run Release -f bump=auto` (patch, or the highest `Release: minor|major` line
  since the last tag; a minor or major needs a `Release-note: …` line too).
[[- if .Images ]]
  Images are built, scanned and signed on GitHub only, never from a laptop.
[[- end ]]
- Commit subjects + PR titles: Go style `area: what` (go.dev/doc/contribute), gated by ship; release notes group
  them by area.
<!-- ship:end -->
