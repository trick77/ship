package cover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const src = `package x

// Doc comment.
func F(a int) int {
	// own-line comment
	if a > 0 {

		return a // trailing
	}
	s := "http://e.com"
	/* block
	   comment */
	return len(s)
}
`

func writeSrc(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backend", "x", "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGoProfile_whenBlocks_thenCodeLinesOnly(t *testing.T) {
	root := writeSrc(t)
	// Block 1: lines 4-7 run; block 2: lines 8-9 never ran; block 3: lines 10-14 ran. Duplicate of block 2 from a
	// second test binary, counted: covered in any wins. Foreign module and zero-statement blocks ignored.
	prof := `mode: atomic
example.com/m/x/x.go:4.20,6.12 2 1
example.com/m/x/x.go:6.12,9.3 1 0
example.com/m/x/x.go:6.12,9.3 1 3
example.com/m/x/x.go:10.2,14.2 2 0
example.com/m/x/x.go:13.2,13.15 0 5
other.com/y/y.go:1.1,2.2 1 1
`
	l, err := GoProfile(strings.NewReader(prof), "example.com/m", "backend", root)
	if err != nil {
		t.Fatal(err)
	}
	got := l["backend/x/x.go"]
	want := map[int]bool{4: true, 6: true, 8: true, 9: true, 10: false, 13: false, 14: false}
	if len(got) != len(want) {
		t.Fatalf("lines %v, want %v", got, want)
	}
	for n, c := range want {
		if got[n] != c {
			t.Errorf("line %d covered=%v, want %v (all %v)", n, got[n], c, got)
		}
	}
	if len(l) != 1 {
		t.Fatalf("files %v", l)
	}
}

func TestGoProfile_whenSourceMissing_thenEveryBlockLineKept(t *testing.T) {
	l, err := GoProfile(strings.NewReader("mode: set\nm/a.go:1.1,3.2 1 0\n"), "m", ".", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(l["a.go"]) != 3 {
		t.Fatalf("%v", l)
	}
}

func TestGoProfile_whenMalformed_thenError(t *testing.T) {
	for _, p := range []string{
		"m/a.go:1.1,3.2 1 0\n",
		"mode: set\nm/a.go 1 0\n",
		"mode: set\nm/a.go:1.1,3.2 1\n",
		"mode: set\nm/a.go:1.1-3.2 1 0\n",
		"mode: set\nm/a.go:x.1,3.2 1 0\n",
		"mode: set\nm/a.go:1.1,y.2 1 0\n",
		"mode: set\nm/a.go:1,3.2 1 0\n",
		"mode: set\nm/a.go:1.1,3.2 z 0\n",
		"mode: set\nm/a.go:1.1,3.2 1 z\n",
	} {
		if _, err := GoProfile(strings.NewReader(p), "m", ".", t.TempDir()); err == nil {
			t.Errorf("%q: no error", p)
		}
	}
}

func TestLcov_whenRelativeAndAbsolute_thenRepoPaths(t *testing.T) {
	root := t.TempDir()
	in := "TN:\nSF:src/a.ts\nDA:1,1\nDA:2,0\nDA:3,2,abc\nend_of_record\nSF:" +
		filepath.Join(root, "ui", "src", "b.ts") + "\nDA:5,0\nend_of_record\n"
	l, err := Lcov(strings.NewReader(in), "ui", root)
	if err != nil {
		t.Fatal(err)
	}
	if a := l["ui/src/a.ts"]; !a[1] || a[2] || !a[3] || len(a) != 3 {
		t.Fatalf("a: %v", a)
	}
	if b := l["ui/src/b.ts"]; len(b) != 1 || b[5] {
		t.Fatalf("b: %v", b)
	}
}

func TestLcov_whenSymlinkedRootOrOutside_thenResolvedOrError(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "ui", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "ui", "src", "a.ts"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Report written through the symlink, root as git reports it (resolved), and the other way round.
	for _, tc := range [][2]string{{link, real}, {real, link}} {
		in := "SF:" + filepath.Join(tc[0], "ui", "src", "a.ts") + "\nDA:1,1\nend_of_record\n"
		l, err := Lcov(strings.NewReader(in), "ui", tc[1])
		if err != nil || !l["ui/src/a.ts"][1] {
			t.Errorf("report %s root %s: %v %v", tc[0], tc[1], l, err)
		}
	}
	in := "SF:" + filepath.Join(t.TempDir(), "elsewhere.ts") + "\nDA:1,1\nend_of_record\n"
	if _, err := Lcov(strings.NewReader(in), "ui", real); err == nil || !strings.Contains(err.Error(), "outside the repo") {
		t.Fatalf("outside: %v", err)
	}
}

func TestLcov_whenRelativePathLeavesRepo_thenError(t *testing.T) {
	in := "SF:../../lib/x.ts\nDA:1,1\nend_of_record\n"
	if _, err := Lcov(strings.NewReader(in), "ui", t.TempDir()); err == nil || !strings.Contains(err.Error(), "outside the repo") {
		t.Fatalf("err = %v", err)
	}
}

func TestLcov_whenMalformed_thenError(t *testing.T) {
	for _, in := range []string{"DA:1,1\n", "SF:a\nDA:1\n", "SF:a\nDA:x,1\n", "SF:a\nDA:1,y\n"} {
		if _, err := Lcov(strings.NewReader(in), "ui", "/r"); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestExcluded(t *testing.T) {
	ex := []string{"cmd", "./gen/", "", "internal/zz_generated.go"}
	for p, want := range map[string]bool{
		"cmd/x/main.go":              true,
		"backend/cmd/x/main.go":      true,
		"gen/a.go":                   true,
		"internal/cmdline/a.go":      false,
		"cmdutil/a.go":               false,
		"internal/zz_generated.go":   true,
		"x/internal/zz_generated.go": true,
		"internal/zz_generated.go2":  false,
		"internal/a.go":              false,
	} {
		if got := Excluded(p, ex); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestFloor(t *testing.T) {
	l := Lines{"a.go": {1: true, 2: true, 3: true, 4: false}}
	if err := Floor("go", l, 75); err != nil {
		t.Fatal(err)
	}
	if err := Floor("go", l, 75.04); err != nil {
		t.Fatalf("within grace: %v", err)
	}
	if err := Floor("go", l, 80); err == nil || !strings.Contains(err.Error(), "75.0%") {
		t.Fatalf("err = %v", err)
	}
	if err := Floor("go", Lines{}, 99); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

func TestPatch_whenChanged_thenOnlyInstrumentedLinesCount(t *testing.T) {
	l := Lines{
		"a.go": {1: true, 2: false, 3: false, 4: false, 7: true},
		"b.ts": {1: false},
	}
	changed := map[string][]int{
		"a.go":      {1, 2, 3, 4, 5, 6}, // 5, 6: comments, not instrumented
		"a_test.go": {1, 2},             // in no report
		"README.md": {1},
	}
	p := PatchOf(l, changed)
	if p.Covered != 1 || p.Total != 4 {
		t.Fatalf("%+v", p)
	}
	err := p.Gate(75)
	if err == nil || !strings.Contains(err.Error(), "a.go: missing lines 2-4") || !strings.Contains(err.Error(), "25.0%") {
		t.Fatalf("err = %v", err)
	}
	if err := p.Gate(25); err != nil {
		t.Fatal(err)
	}
	if err := PatchOf(l, map[string][]int{"README.md": {1}}).Gate(75); err != nil {
		t.Fatalf("n/a: %v", err)
	}
}

func TestMergeAndWithout(t *testing.T) {
	a := Lines{"x.go": {1: false}}
	a.Merge(Lines{"x.go": {1: true, 2: false}, "cmd/m.go": {1: false}})
	if !a["x.go"][1] || a["x.go"][2] || len(a) != 2 {
		t.Fatalf("%v", a)
	}
	if w := a.Without([]string{"cmd/"}); len(w) != 1 {
		t.Fatalf("%v", w)
	}
}

func TestRanges(t *testing.T) {
	if got := ranges([]int{1, 2, 3, 5, 7, 8}); got != "1-3, 5, 7-8" {
		t.Fatal(got)
	}
}

func TestCodeLines_whenScanError_thenNil(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.go")
	if err := os.WriteFile(p, []byte("package x\nvar s = \"unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if codeLines(p) != nil {
		t.Fatal("scan error must keep every line")
	}
}
