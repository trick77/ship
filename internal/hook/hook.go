// Package hook installs and runs the pre-push gate: nothing reaches origin without passing ship check here first.
package hook

import (
	"bufio"
	"errors"
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
// are made via the API). Anything else needs the checked-out branch to have a ship.yaml: a branch cut before ship
// came in is refused, rebase it. The gate is always the newest ship, master's head (SHIP_CMD overrides the command).
const script = `#!/bin/sh
# Installed by ship hooks (github.com/trick77/ship). Runs the newest ship pre-push on this tree.
refs="$(cat)"
gate=0
while read -r local_ref local_sha remote_ref _; do
	[ -z "$local_ref" ] && continue
	case "$local_sha" in *[!0]*) ;; *) continue ;; esac # a delete: all zeros, 40 (SHA-1) or 64 (SHA-256)
	case "$remote_ref" in refs/tags/*) continue ;; esac # what is updated decides, not the local name
	gate=1
done <<EOF
$refs
EOF
[ "$gate" = 1 ] || exit 0
# git runs hooks from the top of the work tree, with GIT_DIR set in a linked worktree (or by --git-dir), possibly
# relative, GIT_WORK_TREE unset or relative, other repo paths possibly relative. Pin them, absolute, so ship's git
# calls from any directory see this tree (ship pre-push also pins GIT_WORK_TREE itself, for older hooks).
top="$(git rev-parse --show-toplevel)" || { echo "pre-push: ship gates pushes from a work tree; this repo has none (bare or mirror clone)"; exit 1; }
if [ -n "$GIT_DIR" ]; then
	GIT_DIR="$(git rev-parse --absolute-git-dir)" || exit 1
	GIT_WORK_TREE="$top"
	export GIT_DIR GIT_WORK_TREE
fi
for var in GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY; do
	eval "val=\${$var}"
	case "$val" in "" | /*) ;; *) eval "$var=\"\$PWD/\$val\"; export $var" ;; esac
done
cd "$top" || exit 1
if ! git ls-files --error-unmatch -- ship.yaml >/dev/null 2>&1; then
	echo "pre-push: this branch has no ship.yaml (cut before ship): rebase it onto the base branch"
	exit 1
fi
# Always the newest ship: master, as the reusable workflows consumers call @master run it, so the hook and CI gate
# alike. Its sha is asked of GitHub by git (a proxy lags on @master; go list would run inside this module and fail
# on its vendor/, go.work or go line), then run by sha through the proxy, which fetches it on demand.
if [ -n "$SHIP_CMD" ]; then
	printf '%s\n' "$refs" | $SHIP_CMD pre-push "$@"
else
	# Exactly refs/heads/master (ls-remote matches by suffix), a 40-hex sha or nothing runs, as in the workflows.
	sha="$(git ls-remote https://github.com/trick77/ship refs/heads/master | awk '$2 == "refs/heads/master" { print $1 }')"
	printf '%s\n' "$sha" | grep -Eqx '[0-9a-f]{40}' ||
		{ echo "pre-push: cannot resolve ship's master on GitHub: '$sha' (git ls-remote, error above)"; exit 1; }
	printf '%s\n' "$refs" | go run "github.com/trick77/ship@$sha" pre-push "$@"
fi
`

// Install writes the pre-push hook where git looks for it (core.hooksPath respected). An existing hook ship did not
// install is never overwritten.
func Install(root string) (string, error) {
	p, err := path(root)
	if err != nil {
		return "", err
	}
	// A core.hooksPath outside this repo (a global or shared hooks dir) would gate every repo using it, and refuse
	// every push of a repo without ship.
	common, err := gitx.Git(root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if _, ok := gitx.Inside(common, p); !ok {
		if _, inTree := gitx.Inside(root, p); inTree {
			return "", fmt.Errorf("pre-push hook path %s is in the working tree (core.hooksPath, e.g. husky or a "+
				"tracked hooks dir): ship never writes tracked files; add ship pre-push to that hook by hand, run at master's sha as the installed hook does (see README)", p)
		}
		return "", fmt.Errorf("pre-push hook path %s is outside this repo (core.hooksPath): installing there would "+
			"gate every repo sharing it; unset core.hooksPath for this repo or add the hook to that dir by hand", p)
	}
	if b, err := os.ReadFile(p); err == nil && !ours(string(b)) { //nolint:gosec // the hook path git reports
		return "", fmt.Errorf("%s exists and ship did not install it: merge it by hand", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	// A new file renamed over the old, never written in place: sh reads a running hook as it goes, and the hook
	// running this very ship (Refresh) would read on into the new script's bytes.
	tmp, err := os.CreateTemp(filepath.Dir(p), ".pre-push-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(script); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // a git hook must be executable
		return "", err
	}
	return p, os.Rename(tmp.Name(), p)
}

// Backup suffixes the previous hook Refresh keeps (pre-push.ship-old), one copy, the last one replaced.
const Backup = ".ship-old"

// Refresh reinstalls a stale hook (Stale) with this ship's script, as Install does, keeping the old one at its path +
// Backup, and reports whether it did. A hook ship did not install is never touched; nor is any hook
// while SHIP_CMD is set: that ship (a local build, a pin) is not master's, whose script the hook must carry, so a
// stale hook is an error to warn about.
func Refresh(root string) (string, bool, error) {
	if !Stale(root) {
		return "", false, nil
	}
	if os.Getenv("SHIP_CMD") != "" {
		return "", false, errors.New("SHIP_CMD runs a ship other than master's, which only warns: rerun `ship hooks` " +
			"with master's (see README)")
	}
	p, err := path(root)
	if err != nil {
		return "", false, err
	}
	old, err := os.ReadFile(p) //nolint:gosec // the hook path git reports
	if err != nil {
		return "", false, err
	}
	// Removed first: a symlink planted at the backup's path is replaced, never written through.
	if err := os.Remove(p + Backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.WriteFile(p+Backup, old, 0o600); err != nil {
		return "", false, err
	}
	p, err = Install(root)
	return p, err == nil, err
}

func ours(s string) bool { return strings.Contains(s, marker) }

// path is where git looks for the pre-push hook, absolute.
func path(root string) (string, error) {
	p, err := gitx.Git(root, "rev-parse", "--git-path", "hooks/pre-push")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p, nil
}

// Stale says whether the installed hook is ship's but not this ship's script: the hook is a copy made by ship hooks
// while ship itself runs unpinned, so a clone keeps an old copy until someone reinstalls it. A hook ship did not
// install, or none, is never stale.
func Stale(root string) bool {
	p, err := path(root)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(p) //nolint:gosec // the hook path git reports
	return err == nil && ours(string(b)) && string(b) != script
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
		if isZero(r.LocalSHA) || strings.HasPrefix(r.Remote, "refs/tags/") {
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
