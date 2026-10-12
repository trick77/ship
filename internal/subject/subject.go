// Package subject reads Go-style commit subjects (go.dev/doc/contribute): `area: what`, several areas comma
// separated (`net/http, net/url: fix …`). The area says where a change is; ship checks the shape and groups
// release notes by it.
package subject

import (
	"fmt"
	"regexp"
	"strings"
)

// Max is the longest subject, a trailing squash-merge ` (#N)` not counted.
const Max = 76

var (
	area   = `[A-Za-z0-9][A-Za-z0-9._/+-]*`
	shape  = regexp.MustCompile(`^(` + area + `(?:, ?` + area + `)*): (\S.*)$`)
	prNum  = regexp.MustCompile(` \(#\d+\)$`)
	revert = regexp.MustCompile(`^Revert "(.*)"$`)
)

// Parse splits s into its areas and text; ok is false when s is not `area: what`.
func Parse(s string) (areas []string, text string, ok bool) {
	m := shape.FindStringSubmatch(s)
	if m == nil {
		return nil, "", false
	}
	for _, a := range strings.Split(m[1], ",") {
		areas = append(areas, strings.TrimSpace(a))
	}
	return areas, m[2], true
}

// Check reports what makes s no Go-style subject. git's own `Revert "…"` and the fixup!/squash!/amend! commits
// rebase --autosquash folds away pass when what they wrap does.
func Check(s string) error {
	for _, p := range []string{"fixup! ", "squash! ", "amend! "} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return Check(rest)
		}
	}
	if m := revert.FindStringSubmatch(s); m != nil {
		return Check(m[1])
	}
	_, text, ok := Parse(s)
	switch {
	case !ok:
		return fmt.Errorf("want `area: what`, e.g. `hook: pin GIT_DIR before cd`")
	case strings.HasSuffix(text, "."):
		return fmt.Errorf("no period at the end")
	}
	if n := len([]rune(prNum.ReplaceAllString(s, ""))); n > Max {
		return fmt.Errorf("%d chars, at most %d", n, Max)
	}
	return nil
}
