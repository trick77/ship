package check

import (
	"errors"
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

// images are docs only outside ui.dir: anything inside is build input.
var images = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true, ".webp": true}

// docsOnly reports whether every path the branch changes against origin/<base> is a doc (isDoc) that no code reads
// (readByCode). Nothing changed is not docs only: CI's run on master itself diffs HEAD against HEAD and must stay the
// whole gate.
func (r runner) docsOnly() (bool, error) {
	base, _, err := r.fork()
	if err != nil {
		return false, err
	}
	files, err := gitx.ChangedFiles(r.root, base)
	if err != nil || len(files) == 0 {
		return false, err
	}
	if slices.ContainsFunc(files, func(f string) bool { return !r.isDoc(f) }) {
		return false, nil
	}
	if by, err := r.readByCode(files); err != nil || by != "" {
		if by != "" {
			fmt.Printf("ship: only docs changed, but %s reads one: whole gate\n", by)
		}
		return false, err
	}
	fmt.Printf("ship: docs only (%d files): build, lint, tests and coverage skipped\n", len(files))
	return true, nil
}

// isDoc reports whether p (repo-relative, as git spells it) is a doc by its path: LICENSE, Markdown or an image,
// outside ui.dir and ui.embed (a UI imports its .md and images, the embed is the Go side's build input) and outside
// testdata/ (tests read it: kit/schemadoc's fixture.md).
func (r runner) isDoc(p string) bool {
	if strings.HasPrefix(p, "testdata/") || strings.Contains(p, "/testdata/") {
		return false
	}
	if u := r.c.UI; u != nil && (under(u.Dir, p) || u.Embed != "" && under(u.Embed, p)) {
		return false
	}
	ext := strings.ToLower(path.Ext(p))
	return path.Base(p) == "LICENSE" || ext == ".md" || images[ext]
}

// under reports whether p lies in dir, both repo-relative and clean; "." holds everything.
func under(dir, p string) bool {
	return dir == "." || p == dir || strings.HasPrefix(p, dir+"/")
}

// readByCode names a file that reads one of docs, "" for none: a file other than Markdown or LICENSE naming a doc
// (a test reading README.md, schemadoc's golden docs/schema.md, a ?raw import, //go:embed by name), or a //go:embed
// pattern matching one (a dir or glob). Tracked and untracked, non-ignored files; a name may match too much, which
// only costs a whole gate.
func (r runner) readByCode(docs []string) (string, error) {
	args := []string{"grep", "-l", "-z", "-I", "-F", "--untracked"}
	for _, d := range docs {
		args = append(args, "-e", path.Base(d))
	}
	named, err := grep(r.root, append(args, "--", ".", ":(exclude,icase)*.md", ":(exclude)LICENSE")...)
	switch {
	case err != nil:
		return "", err
	case len(named) == 1:
		return named[0], nil
	case len(named) > 1:
		return fmt.Sprintf("%s (and %d more)", named[0], len(named)-1), nil
	}
	// -z: "file NUL line", one match per line.
	out, err := gitx.Git(r.root, "grep", "-z", "-I", "--untracked", "-E", `^[[:space:]]*//go:embed[[:space:]]`,
		"--", "*.go")
	if err != nil {
		return "", nilIfNoMatch(err)
	}
	for _, m := range strings.Split(out, "\n") {
		file, line, _ := strings.Cut(m, "\x00")
		for _, d := range docs {
			if embeds(path.Dir(file), line, d) {
				return file + " (//go:embed)", nil
			}
		}
	}
	return "", nil
}

// grep runs git grep -z -l, whose exit 1 is no match, not a failure.
func grep(root string, args ...string) ([]string, error) {
	out, err := gitx.GitZ(root, args...)
	return out, nilIfNoMatch(err)
}

// noMatch reports whether err is git grep's exit 1: nothing matched.
func noMatch(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

func nilIfNoMatch(err error) error {
	if noMatch(err) {
		return nil
	}
	return err
}

// embeds reports whether the //go:embed line of a Go file in dir takes doc: a pattern (quoted or not, all: or not)
// matching doc or a dir above it, which go:embed takes whole.
func embeds(dir, line, doc string) bool {
	_, pats, _ := strings.Cut(strings.TrimSpace(line), "//go:embed")
	for _, p := range strings.Fields(pats) {
		p = strings.TrimPrefix(strings.Trim(p, "\"`"), "all:")
		pat := path.Join(dir, p)
		for d := doc; d != "." && d != "/"; d = path.Dir(d) {
			if ok, _ := path.Match(pat, d); ok {
				return true
			}
		}
	}
	return false
}
