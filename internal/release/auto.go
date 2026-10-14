package release

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

// Auto is the bump that reads the commits since the previous tag: patch, unless a commit message carries a
// `Release: minor` or `Release: major` line (subject.Release). Nothing shipped since: no release.
const Auto = "auto"

// Shipped reports whether a change to path p reaches consumers: docs, tests and fixtures do not. A deny-list, so an
// unknown file releases rather than ships untagged. scaffold's templates are canon whatever their name
// (agents.md, gitignore): ship init writes them into every consumer.
func Shipped(p string) bool {
	switch {
	case strings.HasPrefix(p, "internal/scaffold/templates/"):
		return true
	case strings.HasSuffix(p, ".md"), path.Base(p) == "LICENSE", strings.HasSuffix(p, "_test.go"),
		strings.HasPrefix(p, "testdata/"), strings.Contains(p, "/testdata/"):
		return false
	}
	return true
}

// shippedSince reports whether a file consumers get changed between prev and HEAD, within paths (scope; none: the
// whole repo). --no-renames: a shipped file
// moved into testdata/ is a deletion of a shipped path, which a rename would hide behind its new name.
func shippedSince(root, prev string, paths []string) (bool, error) {
	files, err := gitx.GitZ(root, append([]string{"diff", "--name-only", "--no-renames", "-z", prev, "HEAD", "--"}, paths...)...)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(files, Shipped), nil
}

// shortSHA is a sha cut for messages.
func shortSHA(sha string) string { return sha[:min(12, len(sha))] }

// skipped prints why an auto release does nothing.
func skipped(why string, args ...any) error {
	fmt.Printf("ship: release auto SKIPPED: "+why+"\n", args...)
	return nil
}
