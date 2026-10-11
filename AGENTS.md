# AGENTS.md

Shared gate/hook/release tool, pinned by every trick77 + ipverse Go repo via `go get -tool`. A bug here breaks every
repo's push: change carefully, release deliberately.

- Gate here: `go run . check` before every push (ship is not a go tool of itself: no pre-push hook, run it by hand).
  CI runs automatically here (public: free minutes), full gate on Linux + tests on macOS.
- Release: `go run . release <bump>` (tags `v`-prefixed, Go module). Consumers move by Dependabot; a breaking
  `ship.yaml` or behavior change = minor bump + note in the release.
- Dependencies stay minimal: a tool directive pulls ship's deps into every consumer's go.mod. stdlib + yaml only.
  `go 1.24` floor: tool directive; never raise above the oldest consumer's go line.
- Consumers pin reusable workflows by exact tag (`@v0.1.0`), their Dependabot (github-actions) bumps it with
  the gomod pin. Keep workflow inputs backward compatible within v0.
- golangci-lint runs via `go run ...@<ship.yaml lint>`, never the action's release binary (built with an older Go
  than consumers' go.mod: v2.14.0 binary = go1.27.0).
- Coverage: comment/blank lines never count (go/scanner, not a regex). Floors and patch share one line model.
