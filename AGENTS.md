# AGENTS.md

Shared gate/hook/release tool. Consumers run it unpinned, master (`go run github.com/trick77/ship@master`, workflows
`@master`): every release and every master commit reaches every repo's push at once. Change carefully, release
deliberately.

- Gate here: `go run . check` before every push (ship is not a go tool of itself: no pre-push hook, run it by hand).
  CI runs automatically here (public: free minutes), full gate on Linux.
- Release: automatic, CI on every master push (`release auto`: patch; docs/tests-only → none). Breaking
  `ship.yaml` or behavior change = minor: `Release: minor` + `Release-note: …` lines in the commit message, same
  PR (minor without note fails the run). Fallback by hand: `go run . release <bump>` or the Ship release workflow.
- Nested modules `kit/`, `kit/auth/`: own tags `kit/vX.Y.Z`, `kit/auth/vX.Y.Z`, CI `release auto -dir <m>` per
  module, only that module's commits (and their `Release:` lines) count; first auto run cuts v0.1.0. Module list =
  ship.yaml `go.modules` (Parse cleans entries, `./kit/auth/` is `kit/auth`): CI's release matrix and
  ship-release's dispatch options are literals, a test keeps both equal to the cleaned list.
- Master-sha resolver (`git ls-remote … refs/heads/master | awk …`) runs before ship, so it is copied (check.yaml,
  release.yaml, vuln.yaml, hook.go, Makefile template, README); a test keeps the copies one. Hook script bytes
  changed = every clone's hook rewritten on its next push.
- master is live: the reusable workflows are consumed `@master`. Merge to master only what is releasable; keep
  workflow inputs and ship.yaml backward compatible.
- Dependencies stay minimal (stdlib + yaml): `go run ...@master` builds them on every machine. `go 1.24` floor.
- golangci-lint runs via `go run ...@<ship.yaml lint>`, never the action's release binary (built with an older Go
  than consumers' go.mod: v2.14.0 binary = go1.27.0).
- Tool versions ship runs or demands (golangci-lint default, govulncheck, trivy) live in `internal/pins`, nowhere
  else. ship must never go stale, every repo runs it: a version Dependabot cannot read (a `with:` string, a Go
  const) needs an updater. cosign's `cosign-release: v3.1.3` sat stale until its download 504ed
  and every image release failed (2026-11): cosign = cosign-installer's default now, a test forbids the input.
  internal/pins: the Pins workflow (daily) bumps them, gates, lands on master, dispatches the release. A new pin →
  add it to `tracked` (internal/pins/update). Auto-landed bumps reach every consumer the same day, unchecked
  against their code (lint findings, trivy CVEs): wanted, a stale pin broke them all at once instead. ship's own Dependabot runs daily (public: free minutes).
  npm versions never: Dependabot moves them per repo, an equality pin turns every bump PR red. conform wants the
  UI tools present at an exact version (save-exact), typescript < `pins.TypescriptCap` (typescript-eslint; dependabot's
  ignore renders from it). kit's version is never conform's: Dependabot moves it. Go and Node are floors
  (`pins.GoMin`, `pins.NodeMin`), never equality: Dependabot moves builders up per repo; conform keeps go.mod =
  golang builder and CI node-version = node builder within a repo.
  `internal/check/golangci.yaml` is the family's golangci config, byte-identical to the repos' old copies.
- Commit subjects + PR titles: Go style `area: what` (`hook: …`, `release: …`), gated, and the release notes.
- Conform (`ship conform`, check's first step) is the canon gate: a consumer's owned files are byte-equal to
  internal/scaffold's templates; change the canon there, never in a consumer. Exceptions: ship.yaml `allow` + why.
- Coverage: comment/blank lines never count (go/scanner, not a regex). Floors and patch share one line model.
