package scaffold

import (
	"fmt"
	"strings"
)

// context is how many unchanged lines frame a hunk.
const context = 3

// Diff is a unified diff of have (the file on disk) against want (ship's template), by line. Owned files are short:
// the quadratic LCS is fine.
func Diff(name string, have, want []byte, exists bool) string {
	a, b := lines(have), lines(want)
	from := "a/" + name
	if !exists {
		from = "/dev/null"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ b/%s (ship init)\n", from, name)
	ops := edits(a, b)
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// A hunk: back up over context, run until context+1 unchanged lines in a row (or the end).
		start := max(0, i-context)
		end := i
		for same := 0; end < len(ops) && same <= 2*context; end++ {
			if ops[end].kind == ' ' {
				same++
			} else {
				same = 0
			}
		}
		for end > i && ops[end-1].kind == ' ' && trailing(ops[:end]) > context {
			end--
		}
		h := ops[start:end]
		aStart, bStart := h[0].a+1, h[0].b+1
		var na, nb int
		for _, o := range h {
			if o.kind != '+' {
				na++
			}
			if o.kind != '-' {
				nb++
			}
		}
		if na == 0 {
			aStart--
		}
		if nb == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, na, bStart, nb)
		for _, o := range h {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		i = end
	}
	return out.String()
}

// trailing counts the unchanged lines at the end of ops.
func trailing(ops []op) int {
	n := 0
	for i := len(ops) - 1; i >= 0 && ops[i].kind == ' '; i-- {
		n++
	}
	return n
}

func lines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

type op struct {
	kind byte // ' ', '-' or '+'
	text string
	a, b int // line index in a and b before this op
}

func edits(a, b []string) []op {
	// lcs[i][j]: the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			ops = append(ops, op{' ', a[i], i, j})
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, op{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
