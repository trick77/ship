// Package gitx runs the few git queries ship needs.
package gitx

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Git runs git in dir and returns trimmed stdout; stderr goes into the error.
func Git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...) //nolint:gosec // git with ship's own arguments
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// GitZ runs git with -z output and returns the NUL-separated names verbatim: Git's trimming would eat a leading
// space of the first name.
func GitZ(dir string, args ...string) ([]string, error) {
	cmd := exec.Command("git", args...) //nolint:gosec // git with ship's own arguments
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	var names []string
	for _, n := range strings.Split(out.String(), "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// repoVars point git at one repo; git sets some of them for hooks.
var repoVars = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_QUARANTINE_PATH", "GIT_NAMESPACE"}

// CleanEnv is this process's environment without git's repo variables, plus extra: for the tests and steps check
// runs, whose `git init` + `git commit` in a temp dir would otherwise write into the real repo.
func CleanEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !slices.Contains(repoVars, k) {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// PinWorkTree sets GIT_WORK_TREE when GIT_DIR is set without it, as git leaves it for a linked worktree's hooks:
// git would otherwise take the current directory as the top of the tree, and from a module dir (backend/) every
// file outside it reads as deleted. The top: a linked worktree's git dir names its .git file in "gitdir"; a main
// repo's git dir is <top>/.git. A relative or other GIT_DIR cannot be placed from here: an error says what to do.
func PinWorkTree() error {
	gd := os.Getenv("GIT_DIR")
	if gd == "" || os.Getenv("GIT_WORK_TREE") != "" {
		return nil
	}
	top := ""
	switch {
	case !filepath.IsAbs(gd):
	case fileExists(filepath.Join(gd, "gitdir")):
		b, err := os.ReadFile(filepath.Join(gd, "gitdir")) //nolint:gosec // git's own pointer file
		if err != nil {
			return err
		}
		top = filepath.Dir(strings.TrimSpace(string(b)))
	case filepath.Base(gd) == ".git":
		top = filepath.Dir(gd)
	}
	if top == "" {
		return fmt.Errorf("GIT_DIR=%s without GIT_WORK_TREE: cannot tell the top of the tree from here; "+
			"run go run github.com/trick77/ship@latest hooks, or set GIT_WORK_TREE", gd)
	}
	return os.Setenv("GIT_WORK_TREE", top)
}

func fileExists(p string) bool {
	_, err := os.Stat(p) //nolint:gosec // a path inside GIT_DIR, which git itself set
	return err == nil
}

// Root is the repo's top-level directory containing dir.
func Root(dir string) (string, error) {
	return Git(dir, "rev-parse", "--show-toplevel")
}

// Dirty lists uncommitted changes and untracked, non-ignored files (git status --short), empty when clean.
func Dirty(root string) (string, error) {
	return Git(root, "status", "--short")
}

// Head is the full sha of HEAD.
func Head(root string) (string, error) { return Git(root, "rev-parse", "HEAD") }

// MergeBase is the fork point of HEAD from ref.
func MergeBase(root, ref string) (string, error) { return Git(root, "merge-base", ref, "HEAD") }

// SkipMarker reports whether a commit between base and HEAD carries marker in its message.
func SkipMarker(root, base, marker string) (bool, error) {
	log, err := Git(root, "log", "--format=%B", base+"..HEAD")
	if err != nil {
		return false, err
	}
	return strings.Contains(log, marker), nil
}

// Changed returns the lines the working tree adds or changes against base, per repo-relative path: committed,
// staged and unstaged edits plus every line of untracked, non-ignored files. Deleted lines are not lines.
func Changed(root, base string) (map[string][]int, error) {
	// -M: a moved or renamed file counts only its edited lines, as diff-cover did; the +++ header names the new
	// path. Explicit prefixes: diff.noprefix or diff.mnemonicPrefix in a user's config would otherwise rename every path
	// and the gate would match nothing, passing untested code.
	// core.quotePath=false keeps non-ASCII names unquoted; names git still quotes are unquoted in diffPath.
	diff, err := Git(root, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "-U0", "--inter-hunk-context=0", "-M",
		"--src-prefix=a/", "--dst-prefix=b/", base)
	if err != nil {
		return nil, err
	}
	out, err := parseDiff(diff)
	if err != nil {
		return nil, err
	}
	// -z: names verbatim, NUL-separated; without it git quotes names with special characters.
	untracked, err := GitZ(root, "ls-files", "-z", "-o", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	for _, f := range untracked {
		n, err := countLines(filepath.Join(root, f))
		if err != nil {
			continue // vanished or unreadable: nothing to cover
		}
		for i := 1; i <= n; i++ {
			out[f] = append(out[f], i)
		}
	}
	return out, nil
}

// parseDiff reads a -U0 unified diff: "+++ b/<path>" names the file, "@@ -a,b +c,d @@" opens a hunk. Changed lines
// are the hunk's "+" lines, numbered from c; context lines (diff.interHunkContext can merge hunks across them) are
// counted but never changed. Hunk bodies are walked by their line counts, never by their text: a removed "-- x"
// and an added "++ y" read "--- x" / "+++ y" and would otherwise pass for a file header.
func parseDiff(diff string) (map[string][]int, error) {
	out := map[string][]int{}
	cur := ""
	oldLeft, newLeft, newLine := 0, 0, 0
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		if oldLeft > 0 || newLeft > 0 {
			switch {
			case strings.HasPrefix(line, "-"):
				oldLeft--
			case strings.HasPrefix(line, "+"):
				if cur != "" {
					out[cur] = append(out[cur], newLine)
				}
				newLeft--
				newLine++
			case strings.HasPrefix(line, " "):
				oldLeft--
				newLeft--
				newLine++
			}
			continue // "\ No newline at end of file" and the like count for nothing
		}
		switch {
		case strings.HasPrefix(line, "+++ "):
			cur = diffPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "@@ "):
			oldN, start, n, err := hunk(line)
			if err != nil {
				return nil, err
			}
			oldLeft, newLeft, newLine = oldN, n, start
		}
	}
	return out, sc.Err()
}

// diffPath turns a "+++ " header name into a repo path, "" for /dev/null. git appends a tab to a name containing a
// space and C-quotes one with a quote, backslash or control character ("b/a\"b.go"); both are undone here.
func diffPath(h string) string {
	h = strings.TrimSuffix(h, "\t")
	if strings.HasPrefix(h, `"`) {
		if u, err := strconv.Unquote(h); err == nil {
			h = u
		}
	}
	if h == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(h, "b/")
}

// hunk parses "@@ -a,b +c,d @@": the removed count b, the added start c and count d; counts default to 1.
func hunk(h string) (oldN, start, n int, err error) {
	bad := fmt.Errorf("diff: malformed hunk %q", h)
	f := strings.Fields(h)
	if len(f) < 3 || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return 0, 0, 0, bad
	}
	_, oldN, err = rangeOf(strings.TrimPrefix(f[1], "-"))
	if err != nil {
		return 0, 0, 0, bad
	}
	start, n, err = rangeOf(strings.TrimPrefix(f[2], "+"))
	if err != nil {
		return 0, 0, 0, bad
	}
	return oldN, start, n, nil
}

// rangeOf parses "c,d" or "c" (d = 1).
func rangeOf(s string) (start, n int, err error) {
	a, c, hasCount := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil {
		return 0, 0, err
	}
	n = 1
	if hasCount {
		n, err = strconv.Atoi(c)
	}
	return start, n, err
}

func countLines(path string) (int, error) {
	b, err := os.ReadFile(path) //nolint:gosec // a file git lists in the repo
	if err != nil {
		return 0, err
	}
	if len(b) == 0 {
		return 0, nil
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n, nil
}

// Files is git's view of the tree: tracked and untracked, non-ignored files, as git spells them, optionally limited
// to pathspecs.
func Files(root string, pathspecs ...string) (map[string]bool, error) {
	out, err := GitZ(root, append([]string{"ls-files", "-z", "-co", "--exclude-standard", "--"}, pathspecs...)...)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(out))
	for _, f := range out {
		set[f] = true
	}
	return set, nil
}

// GoFiles lists tracked and untracked, non-ignored .go files that exist in the working tree.
func GoFiles(root string) ([]string, error) {
	set, err := Files(root, "*.go")
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range set {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	return files, nil
}

// Inside reports whether p lies under dir, both resolved (Resolve): never via a "../" path.
func Inside(dir, p string) (rel string, ok bool) {
	rel, err := filepath.Rel(Resolve(dir), Resolve(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, `..\`) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Resolve resolves symlinks in the deepest existing ancestor of p and keeps the rest: the file (or its dirs) may
// be gone by the time the report is read.
func Resolve(p string) string {
	p = filepath.Clean(p)
	for dir, rest := p, ""; ; {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}
