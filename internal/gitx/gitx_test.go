package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Repo makes a git repo in a temp dir with one commit of files, for tests.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "master"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
		{"config", "core.hooksPath", "/dev/null"},
	} {
		mustGit(t, dir, args...)
	}
	write(t, dir, files)
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, s := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChanged_whenCommittedStagedUnstagedUntracked_thenAddedLines(t *testing.T) {
	dir := repo(t, map[string]string{"a.go": "1\n2\n3\n4\n", "gone.go": "x\n"})
	base := mustGit(t, dir, "rev-parse", "HEAD")
	write(t, dir, map[string]string{"a.go": "1\nTWO\n3\n4\n5\n"})
	mustGit(t, dir, "commit", "-qam", "change")
	write(t, dir, map[string]string{"a.go": "ONE\nTWO\n3\n4\n5\n", "new.go": "a\nb", "ignored.log": "x\n", ".gitignore": "*.log\n"})
	mustGit(t, dir, "rm", "-q", "gone.go")

	got, err := Changed(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got["a.go"], []int{1, 2, 5}) {
		t.Errorf("a.go %v", got["a.go"])
	}
	if !slices.Equal(got["new.go"], []int{1, 2}) {
		t.Errorf("new.go %v", got["new.go"])
	}
	if _, ok := got["ignored.log"]; ok {
		t.Error("ignored file counted")
	}
	if _, ok := got["gone.go"]; ok {
		t.Error("deleted file counted")
	}
	if !slices.Equal(got[".gitignore"], []int{1}) {
		t.Errorf(".gitignore %v", got[".gitignore"])
	}
}

func TestRepoQueries(t *testing.T) {
	dir := repo(t, map[string]string{"a.go": "package a\n", "b.txt": "x\n"})
	root, err := Root(filepath.Join(dir))
	if err != nil {
		t.Fatal(err)
	}
	if r1, _ := filepath.EvalSymlinks(root); r1 != mustEval(t, dir) {
		t.Fatalf("root %s, want %s", root, dir)
	}
	if d, _ := Dirty(dir); d != "" {
		t.Fatalf("dirty %q", d)
	}
	write(t, dir, map[string]string{"c.go": "package a\n"})
	if d, _ := Dirty(dir); !strings.Contains(d, "c.go") {
		t.Fatalf("dirty %q", d)
	}
	files, err := GoFiles(dir)
	if err != nil || !slices.Equal(files, []string{"a.go", "c.go"}) {
		t.Fatalf("files %v err %v", files, err)
	}
	if err := os.Remove(filepath.Join(dir, "a.go")); err != nil {
		t.Fatal(err)
	}
	if files, _ := GoFiles(dir); !slices.Equal(files, []string{"c.go"}) {
		t.Fatalf("deleted file listed: %v", files)
	}
	head, _ := Head(dir)
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "x [skip patch-coverage]")
	if skip, err := SkipMarker(dir, head, "[skip patch-coverage]"); err != nil || !skip {
		t.Fatalf("skip %v err %v", skip, err)
	}
	if mb, err := MergeBase(dir, head); err != nil || mb != head {
		t.Fatalf("merge-base %s err %v", mb, err)
	}
	if _, err := Git(dir, "no-such-command"); err == nil {
		t.Fatal("git error swallowed")
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHunk(t *testing.T) {
	for h, want := range map[string][3]int{
		"@@ -1,2 +3,4 @@":  {2, 3, 4},
		"@@ -1 +3 @@ func": {1, 3, 1},
		"@@ -5,2 +4,0 @@":  {2, 4, 0},
	} {
		o, s, n, err := hunk(h)
		if err != nil || o != want[0] || s != want[1] || n != want[2] {
			t.Errorf("%s: %d %d,%d %v", h, o, s, n, err)
		}
	}
	for _, h := range []string{"@@ -1", "@@ -1 3 @@", "@@ 1 +3 @@", "@@ -x +3 @@", "@@ -1 +x @@", "@@ -1 +1,y @@"} {
		if _, _, _, err := hunk(h); err == nil {
			t.Errorf("%s: no error", h)
		}
	}
	if _, err := parseDiff("--- a/a.go\n+++ b/a.go\n@@ bad\n"); err == nil {
		t.Error("bad hunk accepted")
	}
	// A removed "-- x" and an added "++ y" read "--- x" / "+++ y": hunk body, not a file header.
	diff := "--- a/a.md\n+++ b/a.md\n@@ -1 +1,2 @@\n--- x\n+++ y\n+z\n\\ No newline at end of file\n" +
		"@@ -5,0 +9 @@\n+w\n--- a/b.md\n+++ b/b.md\n@@ -1,0 +2 @@\n+q\n"
	got, err := parseDiff(diff)
	if err != nil || !slices.Equal(got["a.md"], []int{1, 2, 9}) || !slices.Equal(got["b.md"], []int{2}) || len(got) != 2 {
		t.Errorf("got %v err %v", got, err)
	}
}

func TestChanged_whenUnusualNames_thenVerbatimPaths(t *testing.T) {
	names := []string{"Übersicht.tsx", "a b.go", `q"uote.go`, "tab\tname.go"}
	files := map[string]string{}
	for _, n := range names {
		files[n] = "1\n"
	}
	dir := repo(t, files)
	base := mustGit(t, dir, "rev-parse", "HEAD")
	more := map[string]string{"new ü.go": "x\ny\n"}
	for _, n := range names {
		more[n] = "1\n2\n"
	}
	write(t, dir, more)
	got, err := Changed(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if !slices.Equal(got[n], []int{2}) {
			t.Errorf("%q: %v (all %v)", n, got[n], got)
		}
	}
	if !slices.Equal(got["new ü.go"], []int{1, 2}) {
		t.Errorf("untracked: %v", got)
	}
	gofiles, err := GoFiles(dir)
	if err != nil || len(gofiles) != 4 || !slices.Contains(gofiles, "new ü.go") || !slices.Contains(gofiles, "tab\tname.go") {
		t.Errorf("GoFiles %q %v", gofiles, err)
	}
}

func TestDiffPath(t *testing.T) {
	for in, want := range map[string]string{
		"b/a.go":           "a.go",
		"b/a b.go\t":       "a b.go",
		`"b/q\"uote.go"`:   `q"uote.go`,
		`"b/\303\234.tsx"`: "Ü.tsx",
		"/dev/null":        "",
	} {
		if got := diffPath(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestChanged_whenInterHunkContextConfigured_thenOnlyChangedLines(t *testing.T) {
	body := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	dir := repo(t, map[string]string{"a.go": body})
	base := mustGit(t, dir, "rev-parse", "HEAD")
	mustGit(t, dir, "config", "diff.interHunkContext", "10")
	write(t, dir, map[string]string{"a.go": strings.Replace(strings.Replace(body, "2\n", "TWO\n", 1), "9\n", "NINE\n", 1)})
	got, err := Changed(dir, base)
	if err != nil || !slices.Equal(got["a.go"], []int{2, 9}) {
		t.Fatalf("got %v err %v", got, err)
	}
	// A hunk with context lines inside: only the + lines count, numbered across the context.
	d, err := parseDiff("--- a/x\n+++ b/x\n@@ -1,4 +1,4 @@\n-a\n+A\n b\n c\n-d\n+D\n")
	if err != nil || !slices.Equal(d["x"], []int{1, 4}) {
		t.Fatalf("got %v err %v", d, err)
	}
}

func TestChanged_whenFileMoved_thenOnlyEditedLines(t *testing.T) {
	body := "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n"
	dir := repo(t, map[string]string{"a/x.go": body})
	base := mustGit(t, dir, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "mv", "a/x.go", "b/x.go")
	write(t, dir, map[string]string{"b/x.go": strings.Replace(body, "5\n", "FIVE\n", 1)})
	got, err := Changed(dir, base)
	if err != nil || !slices.Equal(got["b/x.go"], []int{5}) || len(got) != 1 {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestChanged_whenNoprefixConfig_thenPathsStillRepoRelative(t *testing.T) {
	dir := repo(t, map[string]string{"a.go": "1\n"})
	base := mustGit(t, dir, "rev-parse", "HEAD")
	mustGit(t, dir, "config", "diff.noprefix", "true")
	mustGit(t, dir, "config", "diff.mnemonicPrefix", "true")
	write(t, dir, map[string]string{"a.go": "1\n2\n"})
	got, err := Changed(dir, base)
	if err != nil || !slices.Equal(got["a.go"], []int{2}) {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestCountLines(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"e": "", "n": "a\nb\n", "u": "a\nb"})
	for f, want := range map[string]int{"e": 0, "n": 2, "u": 2} {
		if n, err := countLines(filepath.Join(dir, f)); err != nil || n != want {
			t.Errorf("%s: %d %v", f, n, err)
		}
	}
	if _, err := countLines(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file")
	}
}

func TestCleanEnv(t *testing.T) {
	t.Setenv("GIT_DIR", "/real/repo/.git")
	t.Setenv("GIT_WORK_TREE", "/real/repo")
	t.Setenv("GIT_SSH_COMMAND", "ssh") // not a repo pointer: kept
	env := strings.Join(CleanEnv("EXTRA=1"), "\n")
	if strings.Contains(env, "GIT_DIR=") || strings.Contains(env, "GIT_WORK_TREE=") ||
		!strings.Contains(env, "GIT_SSH_COMMAND=ssh") || !strings.HasSuffix(env, "EXTRA=1") {
		t.Fatalf("env:\n%s", env)
	}
}

func TestPinWorkTree(t *testing.T) {
	dir := repo(t, map[string]string{"a.go": "1\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, dir, "worktree", "add", "-q", "-b", "wt", wt)
	wtGitDir := mustGit(t, wt, "rev-parse", "--absolute-git-dir")
	for name, tc := range map[string]struct {
		gitDir, workTree, want string
		err                    bool
	}{
		"no GIT_DIR":         {"", "", "", false},
		"work tree set":      {"/x/.git", "/given", "/given", false},
		"linked worktree":    {wtGitDir, "", wt, false},
		"main repo":          {filepath.Join(dir, ".git"), "", dir, false},
		"relative":           {".git", "", "", true},
		"bare, no work tree": {filepath.Join(dir, "other.git"), "", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GIT_DIR", tc.gitDir)
			t.Setenv("GIT_WORK_TREE", tc.workTree)
			if tc.gitDir == "" {
				_ = os.Unsetenv("GIT_DIR")
			}
			if tc.workTree == "" {
				_ = os.Unsetenv("GIT_WORK_TREE")
			}
			err := PinWorkTree()
			if (err != nil) != tc.err {
				t.Fatalf("err = %v", err)
			}
			if got := os.Getenv("GIT_WORK_TREE"); !tc.err && Resolve(got) != Resolve(tc.want) && got != tc.want {
				t.Fatalf("GIT_WORK_TREE = %q, want %q", got, tc.want)
			}
		})
	}
}
