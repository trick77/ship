package check

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

// secretRe matches the key shapes the family's code talks to: sk-… (OpenAI, Z.ai, MiMo, LiteLLM, Anthropic), tp-…
// (MiMo Token Plan), GitHub tokens, AWS access key ids. The length bound keeps prose like "sk-" from tripping it; the
// word boundary keeps http-… and disk-… out.
var secretRe = regexp.MustCompile(`\b(?:sk|tp)-[A-Za-z0-9_-]{16,}|\bgh[pousr]_[A-Za-z0-9]{36,}|` +
	`\bgithub_pat_[A-Za-z0-9_]{22,}|\bAKIA[0-9A-Z]{16}\b`)

// AllowSecret on a line exempts it from the secret scan: a fixture or a documented example, never a real key.
const AllowSecret = "ship:allow-secret"

// secrets refuses API keys and .env files on their way to origin: the whole tree (tracked and untracked, as check
// tests it), and every line and message the commits not yet on origin/<base> add, since a key added in one commit
// and removed in the next is clean at the tip but still reaches origin in history. Hits print redacted: a CI log
// must not spread the key further.
func (r runner) secrets() error {
	files, err := gitx.Files(r.root)
	if err != nil {
		return err
	}
	var hits []string
	for f := range files {
		if isEnv(f) {
			hits = append(hits, f+": .env in the tree (keys belong in the environment; gitignore it)")
			continue
		}
		b, err := readText(filepath.Join(r.root, f))
		if err != nil {
			return err
		}
		hits = append(hits, scanSecrets(f, b)...)
	}
	base, err := gitx.MergeBase(r.root, "origin/"+r.c.Base)
	if err != nil {
		return err
	}
	h, err := r.historySecrets(base + "..HEAD")
	if err != nil {
		return err
	}
	hits = append(hits, h...)
	if len(hits) > 0 {
		return fmt.Errorf("%d secret-shaped hit(s); rewrite them out (history: git rebase -i origin/%s), or mark a "+
			"fixture line %s:\n  %s", len(hits), r.c.Base, AllowSecret, strings.Join(sortedUnique(hits), "\n  "))
	}
	return nil
}

// historySecrets scans what the commits of rng add: files named .env, added lines, commit messages. Merges count
// with their own changes (first parent); renames show as adds, so a .env renamed into place is caught.
func (r runner) historySecrets(rng string) ([]string, error) {
	var hits []string
	added, err := gitx.Git(r.root, "-c", "core.quotePath=false", "log", "--format=", "--name-only",
		"--diff-filter=A", "--no-renames", "--diff-merges=first-parent", rng)
	if err != nil {
		return nil, err
	}
	for _, f := range strings.Split(added, "\n") {
		if isEnv(f) {
			hits = append(hits, f+": a commit not yet on origin adds .env")
		}
	}
	msgs, err := gitx.Messages(r.root, rng)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		hits = append(hits, scanSecrets("commit message", []byte(strings.TrimLeft(m, "\n")))...)
	}
	patch, err := gitx.Git(r.root, "-c", "core.quotePath=false", "log", "-p", "-U0", "--no-color", "--no-ext-diff",
		"--no-renames", "--diff-merges=first-parent", "--src-prefix=a/", "--dst-prefix=b/", "--format=commit %h", rng)
	if err != nil {
		return nil, err
	}
	return append(hits, scanPatch(patch)...), nil
}

// scanPatch scans the + lines of git log -p output, naming each hit by commit and file. A "+++ " line is a file
// header only right after a "--- " one: an added line "++ x" reads "+++ x" too.
func scanPatch(patch string) []string {
	var hits []string
	commit, file, prev := "", "", ""
	sc := bufio.NewScanner(strings.NewReader(patch))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "commit "):
			commit = strings.TrimPrefix(line, "commit ")
		case strings.HasPrefix(line, "+++ ") && strings.HasPrefix(prev, "--- "):
			file = strings.TrimPrefix(strings.TrimPrefix(line, "+++ "), "b/")
		case strings.HasPrefix(line, "+"):
			if m := secretIn(line); m != "" {
				hits = append(hits, fmt.Sprintf("commit %s %s: %s", commit, file, m))
			}
		}
		prev = line
	}
	return hits
}

// scanSecrets returns name:line: <redacted match> for every unmarked line of b holding a key shape.
func scanSecrets(name string, b []byte) []string {
	var hits []string
	for i, line := range strings.Split(string(b), "\n") {
		if m := secretIn(line); m != "" {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", name, i+1, m))
		}
	}
	return hits
}

// secretIn is the line's first key shape, redacted, "" for none or a marked line.
func secretIn(line string) string {
	m := secretRe.FindString(line)
	if m == "" || strings.Contains(line, AllowSecret) {
		return ""
	}
	return m[:min(len(m), 6)] + "…"
}

// isEnv matches .env at any depth, as a .gitignore `.env` does; .env.example and the like are fine.
func isEnv(f string) bool { return path.Base(f) == ".env" }

// readText reads a regular file's text; binaries (a NUL in the first 8000 bytes, git's test), symlinks and files
// gone since git listed them read as nothing.
func readText(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, nil //nolint:nilerr // vanished or not a file: nothing to scan
	}
	b, err := os.ReadFile(p) //nolint:gosec // a file git lists in the repo
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return nil, nil
	}
	return b, nil
}

func sortedUnique(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}
