package cover

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// GoProfile reads a Go coverprofile into Lines.
//
// The profile records blocks ("lines A..B hold N statements, run C times"), never which lines inside a block are
// statements. Every line of a block counts, minus the lines go/scanner proves carry no code (comments, blanks), so
// a well-explained change is never failed on its prose. Blocks with 0 statements are skipped.
//
// modPath is the module path from go.mod, modDir the module directory relative to the repo root (root is the repo
// root on disk, for reading the sources). Profile entries outside the module are ignored. With -coverpkg a block
// appears once per test binary: covered in any, covered.
func GoProfile(r io.Reader, modPath, modDir, root string) (Lines, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	out := Lines{}
	code := map[string]map[int]bool{}
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			if !strings.HasPrefix(line, "mode:") {
				return nil, fmt.Errorf("coverprofile: first line %q is not a mode line", line)
			}
			continue
		}
		if line == "" {
			continue
		}
		file, startLine, endLine, stmts, count, err := parseBlock(line)
		if err != nil {
			return nil, err
		}
		rel, ok := strings.CutPrefix(file, modPath+"/")
		if !ok || stmts == 0 {
			continue
		}
		p := path.Clean(path.Join(filepath.ToSlash(modDir), rel))
		cl, seen := code[p]
		if !seen {
			cl = codeLines(filepath.Join(root, filepath.FromSlash(p)))
			code[p] = cl
		}
		for n := startLine; n <= endLine; n++ {
			if cl == nil || cl[n] {
				out.add(p, n, count > 0)
			}
		}
	}
	return out, sc.Err()
}

// parseBlock splits "pkg/file.go:12.34,14.2 3 1".
func parseBlock(line string) (file string, start, end, stmts int, count int64, err error) {
	bad := func() (string, int, int, int, int64, error) {
		return "", 0, 0, 0, 0, fmt.Errorf("coverprofile: malformed line %q", line)
	}
	colon := strings.LastIndex(line, ":")
	if colon < 0 {
		return bad()
	}
	file = line[:colon]
	f := strings.Fields(line[colon+1:])
	if len(f) != 3 {
		return bad()
	}
	from, to, ok := strings.Cut(f[0], ",")
	if !ok {
		return bad()
	}
	sl, _, ok1 := strings.Cut(from, ".")
	el, _, ok2 := strings.Cut(to, ".")
	if !ok1 || !ok2 {
		return bad()
	}
	if start, err = strconv.Atoi(sl); err != nil {
		return bad()
	}
	if end, err = strconv.Atoi(el); err != nil {
		return bad()
	}
	if stmts, err = strconv.Atoi(f[1]); err != nil {
		return bad()
	}
	if count, err = strconv.ParseInt(f[2], 10, 64); err != nil {
		return bad()
	}
	return file, start, end, stmts, count, nil
}

// Lcov reads an lcov report (vitest, c8, istanbul) into Lines from its DA: records. SF: paths that are relative
// are taken relative to base (the UI directory, repo-relative); absolute ones are made relative to root.
func Lcov(r io.Reader, base, root string) (Lines, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	out := Lines{}
	cur := ""
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "SF:"):
			sf := strings.TrimPrefix(line, "SF:")
			if filepath.IsAbs(sf) {
				rel, err := filepath.Rel(root, sf)
				if err != nil {
					return nil, fmt.Errorf("lcov: %s outside repo %s", sf, root)
				}
				cur = filepath.ToSlash(rel)
			} else {
				cur = path.Clean(path.Join(filepath.ToSlash(base), filepath.ToSlash(sf)))
			}
		case strings.HasPrefix(line, "DA:"):
			if cur == "" {
				return nil, fmt.Errorf("lcov: DA before SF")
			}
			n, hits, ok := strings.Cut(strings.TrimPrefix(line, "DA:"), ",")
			if !ok {
				return nil, fmt.Errorf("lcov: malformed %q", line)
			}
			hits, _, _ = strings.Cut(hits, ",") // optional checksum
			ln, err1 := strconv.Atoi(n)
			h, err2 := strconv.ParseInt(hits, 10, 64)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("lcov: malformed %q", line)
			}
			out.add(cur, ln, h > 0)
		case line == "end_of_record":
			cur = ""
		}
	}
	return out, sc.Err()
}
