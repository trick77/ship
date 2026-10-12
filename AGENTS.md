# AGENTS.md

Shared gate/hook/release tool. Consumers run it unpinned, master (`go run github.com/trick77/ship@master`, workflows
`@master`): every release and every master commit reaches every repo's push at once. Change carefully, release
deliberately.

- Gate here: `go run . check` before every push (ship is not a go tool of itself: no pre-push hook, run it by hand).
  CI runs automatically here (public: free minutes), full gate on Linux + tests on macOS.
- Release: automatic, CI on every master push (`release auto`: patch; docs/tests-only → none). Breaking
  `ship.yaml` or behavior change = minor: `Release: minor` + `Release-note: …` lines in the commit message, same
  PR (minor without note fails the run). Fallback by hand: `go run . release <bump>` or the Ship release workflow.
- Nested modules `kit/`, `kit/auth/`: own tags `kit/vX.Y.Z`, `kit/auth/vX.Y.Z`, CI `release auto -dir <m>` per
  module, only that module's commits (and their `Release:` lines) count; first auto run cuts v0.1.0.
- master is live: the reusable workflows are consumed `@master`. Merge to master only what is releasable; keep
  workflow inputs and ship.yaml backward compatible.
- Dependencies stay minimal (stdlib + yaml): `go run ...@master` builds them on every machine. `go 1.24` floor.
- golangci-lint runs via `go run ...@<ship.yaml lint>`, never the action's release binary (built with an older Go
  than consumers' go.mod: v2.14.0 binary = go1.27.0).
- Tool versions ship runs or demands (golangci-lint default, govulncheck, prettier, trivy, cosign) live in
  `internal/pins`, nowhere else (cosign also in release.yaml's installer, a test keeps them equal).
  `internal/check/golangci.yaml` is the family's golangci config, byte-identical to the repos' old copies.
- Commit subjects + PR titles: Go style `area: what` (`hook: …`, `release: …`), gated, and the release notes.
- Conform (`ship conform`, check's first step) is the canon gate: a consumer's owned files are byte-equal to
  internal/scaffold's templates; change the canon there, never in a consumer. Exceptions: ship.yaml `allow` + why.
- Coverage: comment/blank lines never count (go/scanner, not a regex). Floors and patch share one line model.
