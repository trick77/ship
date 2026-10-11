// Package hook installs and runs the pre-push gate: nothing reaches origin without passing ship check here first.
package hook

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

// marker identifies a hook ship may overwrite; any other pre-push hook is the user's to merge by hand.
const marker = "Installed by ship hooks"

// script is installed as the pre-push hook. Deletes and tags pass without a gate (nothing to check; release tags
// are made via the API). Anything else needs the checked-out branch to have ship as a go tool: a branch cut before
// ship came in is refused, rebase it.
const script = `#!/bin/sh
# Installed by ship hooks (github.com/trick77/ship). Runs the checked-out branch's ship pre-push.
refs="$(cat)"
gate=0
while read -r local_ref local_sha _ _; do
	[ -z "$local_ref" ] && continue
	case "$local_sha" in *[!0]*) ;; *) continue ;; esac # a delete: all zeros, 40 (SHA-1) or 64 (SHA-256)
	case "$local_ref" in refs/tags/*) continue ;; esac
	gate=1
done <<EOF
$refs
EOF
[ "$gate" = 1 ] || exit 0
cd "$(git rev-parse --show-toplevel)" || exit 1
for gomod in $(git ls-files -- go.mod '*/go.mod'); do
	if grep -qE '^[[:space:]]*(tool[[:space:]]+)?github.com/trick77/ship[[:space:]]*(//.*)?$' "$gomod"; then
		cd "$(dirname "$gomod")" || exit 1
		printf '%s\n' "$refs" | go tool ship pre-push "$@"
		exit $?
	fi
done
echo "pre-push: this branch has no ship tool in go.mod (cut before ship): rebase it onto the base branch (ship.yaml base:, default master)"
exit 1
`

// Install writes the pre-push hook where git looks for it (core.hooksPath respected). An existing hook ship did not
// install is never overwritten.
func Install(root string) (string, error) {
	p, err := gitx.Git(root, "rev-parse", "--git-path", "hooks/pre-push")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	// A core.hooksPath outside this repo (a global or shared hooks dir) would gate every repo using it, and refuse
	// every push of a repo without ship.
	common, err := gitx.Git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !inside(p, common) {
		return "", fmt.Errorf("pre-push hook path %s is outside this repo (core.hooksPath): installing there would "+
			"gate every repo sharing it; unset core.hooksPath for this repo or add the hook to that dir by hand", p)
	}
	if b, err := os.ReadFile(p); err == nil && !ours(string(b)) { //nolint:gosec // the hook path git reports
		return "", fmt.Errorf("%s exists and ship did not install it: merge it by hand", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, []byte(script), 0o755) //nolint:gosec // a git hook must be executable
}

func ours(s string) bool { return strings.Contains(s, marker) }

// inside reports whether p lies under dir, symlinks resolved (macOS /var vs /private/var) even where p does not
// exist yet.
func inside(p, dir string) bool {
	rel, err := filepath.Rel(gitx.Resolve(dir), gitx.Resolve(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Ref is one line git feeds a pre-push hook.
type Ref struct{ Local, LocalSHA, Remote, RemoteSHA string }

// isZero reports git's null object id, which marks a delete: 40 zeros under SHA-1, 64 under SHA-256.
func isZero(sha string) bool { return sha != "" && strings.Trim(sha, "0") == "" }

// ParseRefs reads the pre-push stdin lines.
func ParseRefs(r io.Reader) ([]Ref, error) {
	var refs []Ref
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 4 {
			return nil, fmt.Errorf("pre-push: unexpected ref line %q", sc.Text())
		}
		refs = append(refs, Ref{f[0], f[1], f[2], f[3]})
	}
	return refs, sc.Err()
}

// Gated returns the refs the gate must cover: not deletes, not tags.
func Gated(refs []Ref) []Ref {
	var out []Ref
	for _, r := range refs {
		if isZero(r.LocalSHA) || strings.HasPrefix(r.Local, "refs/tags/") {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Ready checks that the working tree is exactly what gets pushed: check tests the tree, the push sends commits.
// Every gated ref must be HEAD and the tree must be clean.
func Ready(root string, refs []Ref) error {
	head, err := gitx.Head(root)
	if err != nil {
		return err
	}
	for _, r := range refs {
		sha, err := gitx.Git(root, "rev-parse", r.LocalSHA+"^{commit}")
		if err != nil {
			return err
		}
		if sha != head {
			return fmt.Errorf("pushing %s (%s) but HEAD is %s: check out what you push, check tests HEAD",
				r.Local, short(sha), short(head))
		}
	}
	dirty, err := gitx.Dirty(root)
	if err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf("uncommitted changes, check would test them, not the commit pushed:\n%s", dirty)
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
