package release

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

// Auto is the bump that reads the commits since the previous tag: patch, unless a commit message carries a
// `Release: minor` or `Release: major` line (the highest wins). `Release-note: …` lines go above the notes; a minor
// or major needs one, as a behavior change needs its note.
const Auto = "auto"

// errSkip ends an auto release without one: nothing to do is no failure.
var errSkip = errors.New("skip")

var (
	bumpLine = regexp.MustCompile(`(?m)^Release: *(\S*) *$`)
	noteLine = regexp.MustCompile(`(?m)^Release-note: *(.*?) *$`)
)

// Plan is what the commit messages since the previous tag ask for.
type Plan struct {
	Bump  string
	Notes []string
}

// PlanOf reads messages, oldest first.
func PlanOf(messages []string) (Plan, error) {
	p := Plan{Bump: "patch"}
	rank := map[string]int{"patch": 0, "minor": 1, "major": 2}
	for _, m := range messages {
		for _, b := range bumpLine.FindAllStringSubmatch(m, -1) {
			r, ok := rank[b[1]]
			if !ok || b[1] == "patch" {
				return p, fmt.Errorf("`Release: %s`: want minor or major (patch is the default)", b[1])
			}
			if r > rank[p.Bump] {
				p.Bump = b[1]
			}
		}
		for _, n := range noteLine.FindAllStringSubmatch(m, -1) {
			if n[1] != "" {
				p.Notes = append(p.Notes, n[1])
			}
		}
	}
	if p.Bump != "patch" && len(p.Notes) == 0 {
		return p, fmt.Errorf("`Release: %s` without a `Release-note: …` line: a behavior change needs its note", p.Bump)
	}
	return p, nil
}

// Shipped reports whether a change to path p reaches consumers: docs, tests and fixtures do not. A deny-list, so an
// unknown file releases rather than ships untagged.
func Shipped(p string) bool {
	switch {
	case strings.HasSuffix(p, ".md"), path.Base(p) == "LICENSE", strings.HasSuffix(p, "_test.go"),
		strings.HasPrefix(p, "testdata/"), strings.Contains(p, "/testdata/"):
		return false
	}
	return true
}

// auto resolves the auto bump against prev, the previous release's commit: errSkip when nothing shipped since.
func auto(root, prev string) (Plan, error) {
	if prev == "" {
		return Plan{}, fmt.Errorf("release auto needs a previous tag on origin: cut the first one by hand")
	}
	files, err := gitx.GitZ(root, "diff", "--name-only", "-z", prev, "HEAD")
	if err != nil {
		return Plan{}, err
	}
	shipped := false
	for _, f := range files {
		shipped = shipped || Shipped(f)
	}
	if !shipped {
		fmt.Printf("ship: release auto SKIPPED: only docs, tests or fixtures changed since %s\n", prev[:min(12, len(prev))])
		return Plan{}, errSkip
	}
	messages, err := gitx.GitZ(root, "log", "--reverse", "--format=%B%x00", prev+"..HEAD")
	if err != nil {
		return Plan{}, err
	}
	return PlanOf(messages)
}
