// Package subject reads commit messages: Go-style subjects (go.dev/doc/contribute), `area: what`, several areas
// comma separated (`net/http, net/url: fix …`), and the `Release:` lines that size a release. The area says where a
// change is; ship checks the shape and groups release notes by it.
package subject

import (
	"errors"
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

var (
	bumpLine = regexp.MustCompile(`^[ \t]*Release:[ \t]*(\S*)`)
	noteLine = regexp.MustCompile(`^[ \t]*Release-note:[ \t]*(.*?)[ \t]*$`)
)

// Release reads the `Release: minor|major` and `Release-note: …` lines of commit messages (oldest first) below
// their subjects: a subject like `release: …` is no trailer. bump is patch, or the highest asked; notes come in
// commit order and are returned even with err. err: a value that is not minor or major (a typo must never ship a
// breaking change as patch), or a minor/major without a note.
func Release(messages []string) (bump string, notes []string, err error) {
	bump = "patch"
	rank := map[string]int{"minor": 1, "major": 2}
	var errs []error
	for _, m := range messages {
		_, body, _ := strings.Cut(strings.TrimLeft(m, "\n"), "\n")
		for _, line := range strings.Split(strings.ReplaceAll(body, "\r", ""), "\n") {
			if n := noteLine.FindStringSubmatch(line); n != nil {
				if n[1] != "" {
					notes = append(notes, n[1])
				}
				continue
			}
			b := bumpLine.FindStringSubmatch(line)
			if b == nil {
				continue
			}
			v := strings.ToLower(b[1])
			if rank[v] == 0 {
				errs = append(errs, fmt.Errorf("`%s`: want `Release: minor` or `Release: major` (patch is the default)",
					strings.TrimSpace(line)))
				continue
			}
			if rank[v] > rank[bump] {
				bump = v
			}
		}
	}
	if bump != "patch" && len(notes) == 0 {
		errs = append(errs, fmt.Errorf("`Release: %s` without a `Release-note: …` line: a behavior change needs its note", bump))
	}
	return bump, notes, errors.Join(errs...)
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
