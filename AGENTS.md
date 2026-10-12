# AGENTS.md

Shared gate/hook/release tool. Consumers run it unpinned, master (`go run github.com/trick77/ship@master`, workflows
`@master`): every release and every master commit reaches every repo's push at once. Change carefully, release
deliberately.

- Gate here: `go run . check` before every push (ship is not a go tool of itself: no pre-push hook, run it by hand).
  CI runs automatically here (public: free minutes), full gate on Linux + tests on macOS.
- Release: `go run . release <bump>` (tags `v`-prefixed, Go module). Consumers get it on their next run; a
  breaking `ship.yaml` or behavior change = minor bump + note in the release.
- master is live: the reusable workflows are consumed `@master`. Merge to master only what is releasable; keep
  workflow inputs and ship.yaml backward compatible.
- Dependencies stay minimal (stdlib + yaml): `go run ...@master` builds them on every machine. `go 1.24` floor.
- golangci-lint runs via `go run ...@<ship.yaml lint>`, never the action's release binary (built with an older Go
  than consumers' go.mod: v2.14.0 binary = go1.27.0).
- Commit subjects + PR titles: Go style `area: what` (`hook: …`, `release: …`), gated, and the release notes.
- Coverage: comment/blank lines never count (go/scanner, not a regex). Floors and patch share one line model.
