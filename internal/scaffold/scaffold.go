// Package scaffold renders the files ship owns in a consumer repo (workflow callers, dependabot, the UI's ESLint
// config, tsconfig.node.json and .npmrc, the ship blocks in Makefile, .dockerignore, .gitignore and AGENTS.md) from
// ship.yaml and the repo's facts. Owned means byte-equal to the rendered template: a copied file that drifts is
// caught on the next push, never in an audit.
package scaffold

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/pins"
)

// DependabotOnly is the private repo's CI condition: Dependabot PRs only (Actions minutes are billed, own pushes are
// gated by the hook).
const DependabotOnly = "github.event_name != 'pull_request' || github.event.pull_request.user.login == 'dependabot[bot]'"

// Begin and End mark the block ship owns inside a file the repo otherwise keeps (Makefile, .dockerignore,
// .gitignore); AGENTS.md's are HTML comments (a # line is a Markdown heading).
const (
	Begin   = "# ship:begin"
	End     = "# ship:end"
	MDBegin = "<!-- ship:begin -->"
	MDEnd   = "<!-- ship:end -->"
)

// markers are a block's first and last line prefixes.
type markers struct{ begin, end string }

var (
	hashMarks = markers{Begin, End}
	mdMarks   = markers{MDBegin, MDEnd}
)

// Owned file paths, repo-relative.
const (
	CI         = ".github/workflows/ci.yaml"
	Release    = ".github/workflows/release.yaml"
	Cleanup    = ".github/workflows/cleanup-images.yaml"
	Dependabot = ".github/dependabot.yaml"
	Ignore     = ".dockerignore"
	Makefile   = "Makefile"
	GitIgnore  = ".gitignore"
	Agents     = "AGENTS.md"
	// In the UI's directory.
	ESLint       = "eslint.config.js"
	TSConfigNode = "tsconfig.node.json"
	NPMRC        = ".npmrc"
)

//go:embed templates
var templates embed.FS

// Facts is what the templates need beyond ship.yaml, found in the repo or asked of GitHub.
type Facts struct {
	// Repo is the GitHub repository name (origin), the image packages' base name.
	Repo string
	// Visibility is public or private.
	Visibility string
	// DockerDirs are the repo-absolute directories of the images' Containerfiles that exist.
	DockerDirs []string
	// Compose: a compose file at the root names an image by a version tag (a pinned third-party image), which only
	// Dependabot's docker-compose ecosystem bumps.
	Compose bool
	// NodeVersion is the Node major the CI caller sets up for a UI: the Containerfile's node builder's, so CI and
	// the image build alike; pins.NodeMin when there is none.
	NodeVersion string
}

// File is one owned file: what is on disk, what ship wants there, or why it cannot be rendered.
type File struct {
	Path string
	Have []byte
	// Exists is false for a missing file (Have is nil).
	Exists bool
	Want   []byte
	// Manual is set when ship cannot write the file itself (Want is then unset).
	Manual string
}

// Differs reports whether the file needs ship init (or a hand fix).
func (f File) Differs() bool { return f.Manual != "" || !bytes.Equal(f.Have, f.Want) }

type data struct {
	Private, UI, Images, GoDirSet, UIDirSet bool
	Compose                                 bool
	// MakefileOwned: the Makefile carries ship's block, so its gate is make check (else ship check).
	MakefileOwned bool
	// UICoverage is the UI test runner's report dir, anchored (/coverage/ui/): the only coverage ship's gate writes
	// into the tree (Go's lives in the git dir).
	UICoverage                                 string
	Branches, GoDir, GoDirAbs, UIDir, UIDirAbs string
	DependabotOnly, NodeVersion, Packages      string
	TypescriptCap                              string
	DockerDirs, GoModules                      []string
	// Ignore is ship.yaml's dependabot.ignore by ecosystem.
	Ignore map[string][]config.Ignore
}

// Files renders every file ship owns in root. A file whose facts are missing (no origin, no visibility) carries
// Manual instead of Want.
func Files(root string, c *config.Config, f Facts) ([]File, error) {
	d := data{
		Private: f.Visibility == "private", Images: len(c.Images) > 0, UI: c.UI != nil,
		Branches: "[" + c.Base + "]", GoDir: c.Go.Dir, GoDirSet: c.Go.Dir != ".",
		GoDirAbs: abs(c.Go.Dir), DependabotOnly: DependabotOnly, NodeVersion: f.NodeVersion, DockerDirs: f.DockerDirs,
		Compose: f.Compose, Ignore: map[string][]config.Ignore{}, TypescriptCap: pins.TypescriptCap,
	}
	// An ignore under an ecosystem dependabot.yaml has no entry for would be written nowhere, init and conform green.
	rendered := map[string]bool{"gomod": true, "github-actions": true, "npm": d.UI, "docker": len(f.DockerDirs) > 0,
		"docker-compose": f.Compose}
	for _, ig := range c.Dependabot.Ignore {
		if !rendered[ig.Ecosystem] {
			return nil, fmt.Errorf("%s: dependabot.ignore: %s %s, but dependabot.yaml has no %s entry", config.File,
				ig.Ecosystem, ig.Dependency, ig.Ecosystem)
		}
		d.Ignore[ig.Ecosystem] = append(d.Ignore[ig.Ecosystem], ig)
	}
	for _, m := range c.Go.Modules {
		d.GoModules = append(d.GoModules, abs(m))
	}
	if d.NodeVersion == "" {
		d.NodeVersion = pins.NodeMin
	}
	if c.UI != nil {
		d.UIDir, d.UIDirAbs, d.UIDirSet = c.UI.Dir, abs(c.UI.Dir), c.UI.Dir != "ui"
		d.UICoverage = uiCoverage(c.UI.Lcov)
	}
	var files []File
	add := func(p, tmpl, manual string, block blockFunc) error {
		fl := File{Path: p}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))) //nolint:gosec // the repo's own files
		switch {
		case err == nil:
			fl.Have, fl.Exists = b, true
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if manual != "" {
			fl.Manual = manual
			files = append(files, fl)
			return nil
		}
		want, err := render(tmpl, d)
		if err != nil {
			return err
		}
		if block != nil {
			fl.Want, fl.Manual = block(fl.Have, want)
		} else {
			fl.Want = []byte(want)
		}
		files = append(files, fl)
		return nil
	}
	visibility := ""
	if f.Visibility == "" {
		visibility = "repo visibility unknown (set ship.yaml visibility: or fix the lookup), ci.yaml cannot be rendered"
	}
	if err := add(CI, "ci.yaml", visibility, nil); err != nil {
		return nil, err
	}
	if err := add(Release, "release.yaml", "", nil); err != nil {
		return nil, err
	}
	if d.Images {
		pkgs := ""
		if f.Repo == "" {
			pkgs = "no GitHub origin remote: the image package names are unknown"
		}
		d.Packages = packages(f.Repo, c.Images)
		if err := add(Cleanup, "cleanup-images.yaml", pkgs, nil); err != nil {
			return nil, err
		}
	}
	if err := add(Dependabot, "dependabot.yaml", "", nil); err != nil {
		return nil, err
	}
	if d.Images {
		if err := add(Ignore, "dockerignore", "", prepend); err != nil {
			return nil, err
		}
	}
	if d.UI {
		for _, t := range []struct{ name, tmpl string }{{ESLint, "eslint.config.js"},
			{TSConfigNode, "tsconfig.node.json"}, {NPMRC, "npmrc"}} {
			if err := add(path.Join(d.UIDir, t.name), t.tmpl, "", nil); err != nil {
				return nil, err
			}
		}
	}
	// A library without a Makefile needs none; an app (images or a UI) gets the gate's targets.
	if _, err := os.Stat(filepath.Join(root, Makefile)); err == nil || d.Images || d.UI {
		if err := add(Makefile, "Makefile", "", appendBlock(hashMarks, makeTargets)); err != nil {
			return nil, err
		}
		// Owned only once the block merges: a Makefile left by hand has no make check yet, AGENTS.md says ship's.
		d.MakefileOwned = files[len(files)-1].Manual == ""
	}
	if err := add(GitIgnore, "gitignore", "", prepend); err != nil {
		return nil, err
	}
	agents := ""
	if f.Visibility == "" {
		agents = "repo visibility unknown (set ship.yaml visibility: or fix the lookup), AGENTS.md's CI line cannot " +
			"be rendered"
	}
	if err := add(Agents, "agents.md", agents, appendBlock(mdMarks, nil)); err != nil {
		return nil, err
	}
	return files, nil
}

func abs(dir string) string { return path.Join("/", filepath.ToSlash(dir)) }

// uiCoverage is ui.lcov's dir, anchored, for .gitignore: only a report dir in the repo (a coverage segment in its
// path), never a source dir (ui.lcov: ui/lcov.info would ignore /ui/) nor one outside the repo; else none.
func uiCoverage(lcov string) string {
	c, escapes := config.CleanDir(lcov)
	dir := path.Dir(c)
	if escapes || dir == "." || !slices.Contains(strings.Split(dir, "/"), "coverage") {
		return ""
	}
	return abs(dir) + "/"
}

// packages is the cleanup caller's JSON list: ghcr.io/<owner>/<repo><suffix>, as ship release names the images.
func packages(repo string, images []config.Image) string {
	var names []string
	for _, im := range images {
		if n := repo + im.Suffix; !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	b, _ := json.Marshal(names) // a []string always marshals
	return strings.ReplaceAll(string(b), `","`, `", "`)
}

// funcs: quote writes a YAML double-quoted scalar (Go's quoting is a subset of YAML's for single-line strings),
// quoteList a flow sequence of them.
var funcs = template.FuncMap{
	"quote": strconv.Quote,
	"quoteList": func(ss []string) string {
		q := make([]string, len(ss))
		for i, s := range ss {
			q[i] = strconv.Quote(s)
		}
		return "[" + strings.Join(q, ", ") + "]"
	},
}

func render(name string, d data) (string, error) {
	t, err := template.New(name).Delims("[[", "]]").Funcs(funcs).ParseFS(templates, "templates/"+name)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// blockRange finds the owned block: the begin marker's line through the end marker's line, end included. found is
// false when the file has neither marker. A lone marker, an end before its begin or a second begin is broken: the
// block's extent is unknown, so ship neither splices nor appends (a second block would follow the half one).
func blockRange(have []byte, m markers) (start, end int, found bool, broken string) {
	s := string(have)
	begins, ends := lineStarts(s, m.begin), lineStarts(s, m.end)
	switch {
	case len(begins) == 0 && len(ends) == 0:
		return 0, 0, false, ""
	case len(begins) != 1 || len(ends) != 1 || ends[0] < begins[0]:
		return 0, 0, true, fmt.Sprintf("ship block markers broken: restore %s closing the one %s block, or remove "+
			"both markers, then ship init (found %d begin, %d end)", m.end, m.begin, len(begins), len(ends))
	}
	i, j := begins[0], ends[0]
	k := strings.IndexByte(s[j:], '\n')
	if k < 0 {
		return i, len(s), true, ""
	}
	return i, j + k + 1, true, ""
}

// lineStarts are the offsets of the lines starting with prefix.
func lineStarts(s, prefix string) []int {
	var at []int
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], prefix) {
			at = append(at, i)
		}
		n := strings.IndexByte(s[i:], '\n')
		if n < 0 {
			break
		}
		i += n + 1
	}
	return at
}

// replaceBlock swaps the file's block for block. found is false when the file has none; broken says why its
// markers cannot be trusted.
func replaceBlock(have []byte, block string, m markers) (b []byte, found bool, broken string) {
	i, j, found, broken := blockRange(have, m)
	if !found || broken != "" {
		return nil, found, broken
	}
	return slices.Concat(have[:i], []byte(block), have[j:]), true, ""
}

// blockFunc merges the rendered block into the file's bytes, or says why that is by hand.
type blockFunc func(have []byte, block string) ([]byte, string)

// prepend puts the block first, so the repo's own lines after it win: a .dockerignore allowlist (`*`) narrowed by
// the repo, a .gitignore negation (`!/coverage/ui/badge.svg`).
func prepend(have []byte, block string) ([]byte, string) {
	b, found, broken := replaceBlock(have, block, hashMarks)
	switch {
	case broken != "":
		return nil, broken
	case found:
		return b, ""
	case len(have) == 0:
		return []byte(block), ""
	}
	return slices.Concat([]byte(block+"\n"), have), ""
}

// makeTargets: the Makefile's lines the block defines. Targets of the same name outside the block would override
// each other: those are moved by hand.
func makeTargets(have []byte) string {
	for _, l := range strings.Split(string(have), "\n") {
		for _, t := range []string{"SHIP_CMD", "check:", "hooks:", "lint:"} {
			if strings.HasPrefix(l, t) {
				return fmt.Sprintf("Makefile has its own %s outside the %s/%s block: move it in (ship init "+
					"then owns the block)", strings.TrimSuffix(t, ":"), Begin, End)
			}
		}
	}
	return ""
}

// appendBlock adds the block last, so the Makefile's default goal stays the repo's. guard, if set, finds what
// keeps a first block from being appended (Makefile: its own targets).
func appendBlock(m markers, guard func(have []byte) string) blockFunc {
	return func(have []byte, block string) ([]byte, string) {
		b, found, broken := replaceBlock(have, block, m)
		switch {
		case broken != "":
			return nil, broken
		case found:
			return b, ""
		}
		if guard != nil {
			if manual := guard(have); manual != "" {
				return nil, manual
			}
		}
		if len(have) == 0 {
			return []byte(block), ""
		}
		if !bytes.HasSuffix(have, []byte("\n")) {
			have = append(have, '\n')
		}
		return slices.Concat(have, []byte("\n"+block)), ""
	}
}

// Write writes every file that differs and ship can render; it returns the paths written.
func Write(root string, files []File) ([]string, error) {
	var wrote []string
	for _, f := range files {
		if f.Manual != "" || !f.Differs() {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(p, f.Want, 0o644); err != nil { //nolint:gosec // repo files, not secrets
			return wrote, err
		}
		wrote = append(wrote, f.Path)
	}
	return wrote, nil
}

// PrintDiff writes a diff of every differing file to w and reports whether any differs.
func PrintDiff(w io.Writer, files []File) bool {
	differs := false
	for _, f := range files {
		if !f.Differs() {
			continue
		}
		differs = true
		if f.Manual != "" {
			_, _ = fmt.Fprintf(w, "%s: %s\n", f.Path, f.Manual)
			continue
		}
		_, _ = io.WriteString(w, Diff(f.Path, f.Have, f.Want, f.Exists))
	}
	return differs
}
