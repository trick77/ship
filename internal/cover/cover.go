// Package cover turns Go coverprofiles and lcov reports into one line-level model and runs the two gates on it:
// the floor (is the codebase as a whole tested enough?) and the patch gate (is the code this branch changed tested?).
//
// The floor alone lets a large tested codebase absorb untested new code; the patch gate alone lets old debt sit
// forever. Both together pin the gain without punishing a branch for debt it inherited.
package cover

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Lines maps a repo-relative, slash-separated path to its instrumented lines: line number to covered.
type Lines map[string]map[int]bool

func (l Lines) add(path string, line int, covered bool) {
	m := l[path]
	if m == nil {
		m = map[int]bool{}
		l[path] = m
	}
	m[line] = m[line] || covered
}

// Merge adds other's lines; a line covered in either is covered.
func (l Lines) Merge(other Lines) {
	for p, m := range other {
		for n, c := range m {
			l.add(p, n, c)
		}
	}
}

// Without drops every path that matches an exclude prefix (see Excluded).
func (l Lines) Without(exclude []string) Lines {
	out := Lines{}
	for p, m := range l {
		if !Excluded(p, exclude) {
			out[p] = m
		}
	}
	return out
}

// Excluded reports whether path matches one of the prefixes, at the start or as a path segment anywhere:
// "cmd/" excludes cmd/x.go and backend/cmd/x.go, not internal/cmdline/x.go.
func Excluded(path string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.TrimPrefix(filepath.ToSlash(p), "./")
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/" // whole directories: "cmd" must not swallow internal/cmdline
		}
		if strings.HasPrefix(path, p) || strings.Contains(path, "/"+p) {
			return true
		}
	}
	return false
}

// Percent returns covered and total line counts over every file.
func (l Lines) Percent() (covered, total int) {
	for _, m := range l {
		for _, c := range m {
			total++
			if c {
				covered++
			}
		}
	}
	return covered, total
}

// Pct formats covered/total as a percentage; an empty set is 100 (nothing to cover).
func Pct(covered, total int) float64 {
	if total == 0 {
		return 100
	}
	return 100 * float64(covered) / float64(total)
}

// Floor fails when l's line coverage is below floor. The 0.05 grace keeps float formatting alone from failing.
func Floor(name string, l Lines, floor float64) error {
	c, t := l.Percent()
	pct := Pct(c, t)
	if pct < floor-0.05 {
		return fmt.Errorf("%s coverage %.1f%% of lines (%d/%d), floor is %.1f%%", name, pct, c, t, floor)
	}
	fmt.Printf("ship: %s coverage ok: %.1f%% of lines (floor %.1f%%)\n", name, pct, floor)
	return nil
}

// Patch is the patch gate's result: the changed lines that carry code, and which of them are covered.
type Patch struct {
	Covered, Total int
	// Missing lists, per file, the changed code lines no test runs.
	Missing map[string][]int
}

// PatchOf intersects the changed lines with the instrumented ones. Changed lines no report knows (tests, docs,
// comments, files outside every report) carry no executable code and are not counted.
func PatchOf(l Lines, changed map[string][]int) Patch {
	p := Patch{Missing: map[string][]int{}}
	for path, ns := range changed {
		m := l[path]
		for _, n := range ns {
			c, ok := m[n]
			if !ok {
				continue
			}
			p.Total++
			if c {
				p.Covered++
			} else {
				p.Missing[path] = append(p.Missing[path], n)
			}
		}
	}
	for path := range p.Missing {
		slices.Sort(p.Missing[path])
	}
	return p
}

// Gate fails when the patch's coverage is below min, listing the missing lines per file.
func (p Patch) Gate(min float64) error {
	if p.Total == 0 {
		fmt.Println("ship: patch coverage n/a: the diff touches no instrumented lines")
		return nil
	}
	pct := Pct(p.Covered, p.Total)
	if pct >= min-0.05 {
		fmt.Printf("ship: patch coverage ok: %.1f%% of %d changed lines (min %.1f%%)\n", pct, p.Total, min)
		return nil
	}
	var b strings.Builder
	files := make([]string, 0, len(p.Missing))
	for f := range p.Missing {
		files = append(files, f)
	}
	slices.Sort(files)
	for _, f := range files {
		fmt.Fprintf(&b, "\n  %s: missing lines %s", f, ranges(p.Missing[f]))
	}
	return fmt.Errorf("patch coverage %.1f%% of %d changed lines, min %.1f%%:%s", pct, p.Total, min, b.String())
}

// ranges renders sorted line numbers as "3-5, 9".
func ranges(ns []int) string {
	var parts []string
	for i := 0; i < len(ns); {
		j := i
		for j+1 < len(ns) && ns[j+1] == ns[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, fmt.Sprint(ns[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ns[i], ns[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// codeLines returns the 1-based lines of a Go file that carry at least one non-comment token, or nil when the file
// cannot be read or scanned: the caller then keeps every line, since dropping lines it cannot prove are comments
// could hide uncovered code.
//
// go/scanner, not a "//" pattern: `x := "http://e.com"` and `f() // why` stay code, own-line comments, block
// comment interiors and blank lines go. SEMICOLON counts: an implicit one sits on the line ending a statement.
func codeLines(path string) map[int]bool {
	src, err := os.ReadFile(path) //nolint:gosec // a source file named by the coverage report
	if err != nil {
		return nil
	}
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	bad := false
	s.Init(file, src, func(token.Position, string) { bad = true }, scanner.ScanComments)
	lines := map[int]bool{}
	for {
		pos, tok, _ := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.COMMENT {
			lines[file.Position(pos).Line] = true
		}
	}
	if bad {
		return nil
	}
	return lines
}
