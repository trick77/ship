package conform

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/scaffold"
)

// KitModule is ship's library of the code every service had hand-rolled (version, health, graceful shutdown).
const KitModule = "github.com/trick77/ship/kit"

// AgentsLine is the start of AGENTS.md's commit-subject line (README, Adopting ship); the areas are the repo's.
const AgentsLine = "Commit subjects + PR titles: Go style `area: what`"

// listed is git's view of the tree (tracked and untracked, not ignored), read once.
func (e *env) listed() map[string]bool {
	if e.git == nil {
		e.git, _ = gitx.Files(e.root) // no listing: the rules reading it find nothing, the others still run
		if e.git == nil {
			e.git = map[string]bool{}
		}
	}
	return e.git
}

func (e *env) scaffold() result {
	if e.renderErr != nil {
		return result{findings: []finding{{what: e.renderErr.Error(), file: "ship.yaml"}}}
	}
	var fs []finding
	for _, f := range e.files {
		switch {
		case f.Manual != "":
			fs = append(fs, finding{what: f.Manual, file: f.Path})
		case !f.Exists:
			fs = append(fs, finding{what: "missing (ship init)", file: f.Path})
		case f.Differs():
			fs = append(fs, finding{what: "differs from ship's template (ship init -diff)", file: f.Path})
		}
	}
	return result{findings: fs}
}

// dockerignore: the host's node_modules (darwin binaries) and a stale dist never reach the image.
func (e *env) dockerignore() result {
	if len(e.c.Images) == 0 {
		return result{skip: "no images"}
	}
	b, err := e.read(scaffold.Ignore)
	switch {
	case err != nil:
		return result{findings: []finding{{what: err.Error(), file: scaffold.Ignore}}}
	case b == nil:
		return result{findings: []finding{{what: "missing (ship init)", file: scaffold.Ignore}}}
	case e.c.UI == nil:
		return result{}
	}
	lines := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		lines[strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(l), "/"), "/")] = true
	}
	var fs []finding
	for _, d := range []string{"node_modules", "dist"} {
		if !lines[e.uiPath(d)] && !lines["**/"+d] {
			fs = append(fs, finding{what: "does not exclude " + e.uiPath(d), file: scaffold.Ignore})
		}
	}
	return result{findings: fs}
}

// The UI's scripts, as ship.yaml's ui.lint and ui.build run them.
const (
	lintScript   = "eslint . --max-warnings 0 && npm run format:check"
	formatScript = "prettier --check 'src/**/*.{ts,tsx,css}' '*.ts'"
	buildPrefix  = "tsc -b && vite build"
)

// ui: one lint (ESLint, then Prettier at one pinned version), and the build is the typecheck.
func (e *env) ui() result {
	if e.c.UI == nil {
		return result{skip: "no ui"}
	}
	var fs []finding
	if e.c.UI.Lint != "npm run lint" {
		fs = append(fs, finding{what: fmt.Sprintf("ui.lint %q, want npm run lint", e.c.UI.Lint), file: "ship.yaml"})
	}
	pj := e.uiPath("package.json")
	p, err := e.packageJSON()
	switch {
	case err != nil:
		return result{findings: append(fs, finding{what: err.Error(), file: pj})}
	case p == nil:
		return result{findings: append(fs, finding{what: "missing", file: pj})}
	}
	for _, s := range []struct{ name, want string }{{"lint", lintScript}, {"format:check", formatScript}} {
		if got := p.Scripts[s.name]; got != s.want {
			fs = append(fs, finding{what: fmt.Sprintf("script %s %q, want %q", s.name, got, s.want), file: pj})
		}
	}
	if b := p.Scripts["build"]; !strings.HasPrefix(b, buildPrefix) {
		fs = append(fs, finding{what: fmt.Sprintf("script build %q, want it to start with %q (the typecheck)", b,
			buildPrefix), file: pj})
	}
	if v := p.DevDependencies["prettier"]; v != Prettier {
		fs = append(fs, finding{what: fmt.Sprintf("devDependency prettier %q, want exactly %s", v, Prettier), file: pj})
	}
	if es := e.uiPath(scaffold.ESLint); !e.exists(es) {
		fs = append(fs, finding{what: "missing (ship init)", file: es})
	}
	return result{findings: fs}
}

// golangci: one lint config, ship's, everywhere.
func (e *env) golangci() result {
	var fs []finding
	for f := range e.listed() {
		if b := path.Base(f); b == ".golangci.yaml" || b == ".golangci.yml" {
			fs = append(fs, finding{what: "own golangci-lint config: ship's is the family's", file: f})
		}
	}
	slices.SortFunc(fs, func(a, b finding) int { return strings.Compare(a.file, b.file) })
	return result{findings: fs}
}

var goLine = regexp.MustCompile(`^go\s+(\S+)`)

// goMod is the parts of go.mod conform reads.
type goMod struct {
	goVersion string
	kit       string // required version of KitModule, "" when not required
}

func (e *env) goMod() (*goMod, error) {
	b, err := e.read(e.goPath("go.mod"))
	if err != nil || b == nil {
		return nil, err
	}
	m := &goMod{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	block := false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if c := strings.Index(l, "//"); c >= 0 {
			l = strings.TrimSpace(l[:c])
		}
		if g := goLine.FindStringSubmatch(l); g != nil {
			m.goVersion = g[1]
			continue
		}
		f := strings.Fields(l)
		switch {
		case len(f) == 2 && f[0] == "require" && f[1] == "(":
			block = true
			continue
		case block && l == ")":
			block = false
			continue
		case !block && len(f) >= 3 && f[0] == "require":
			f = f[1:]
		case !block:
			continue
		}
		if len(f) >= 2 && f[0] == KitModule {
			m.kit = f[1]
		}
	}
	return m, nil
}

func (e *env) goMajorMinor() (string, error) {
	m, err := e.goMod()
	if err != nil || m == nil {
		return "", err
	}
	parts := strings.SplitN(m.goVersion, ".", 3)
	if len(parts) < 2 {
		return "", nil
	}
	return parts[0] + "." + parts[1], nil
}

var fullGo = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// gomod: the go line is a full toolchain version (go mod tidy's own form), and kit, once required, is the newest.
func (e *env) gomod() result {
	file := e.goPath("go.mod")
	m, err := e.goMod()
	switch {
	case err != nil:
		return result{findings: []finding{{what: err.Error(), file: file}}}
	case m == nil:
		return result{findings: []finding{{what: "missing", file: file}}}
	}
	var fs []finding
	if !fullGo.MatchString(m.goVersion) {
		fs = append(fs, finding{what: fmt.Sprintf("go %s: want x.y.z", m.goVersion), file: file})
	}
	if m.kit == "" {
		return result{findings: fs}
	}
	latest, err := e.latestKit()
	switch {
	case err != nil:
		fs = append(fs, finding{what: err.Error(), file: file})
	case latest == "":
		return result{findings: fs, note: "no kit/v* tag yet"}
	case m.kit != latest:
		fs = append(fs, finding{what: fmt.Sprintf("%s %s, newest is %s (go get %s@%s)", KitModule, m.kit, latest,
			KitModule, latest), file: file})
	}
	return result{findings: fs}
}

// latestKit is the newest kit/v* tag as a module version, "" when there is none.
func (e *env) latestKit() (string, error) {
	tags, err := e.o.KitTags()
	if err != nil {
		return "", err
	}
	best, bestV := "", []int(nil)
	for _, t := range tags {
		v, ok := strings.CutPrefix(t, "kit/")
		if !ok {
			continue
		}
		n, ok := semver(v)
		if ok && (bestV == nil || slices.Compare(n, bestV) > 0) {
			best, bestV = v, n
		}
	}
	return best, nil
}

// semver parses vX.Y.Z (no pre-release: a kit release is a plain tag).
func semver(v string) ([]int, bool) {
	s, ok := strings.CutPrefix(v, "v")
	parts := strings.Split(s, ".")
	if !ok || len(parts) != 3 {
		return nil, false
	}
	n := make([]int, 3)
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		n[i] = x
	}
	return n, true
}

// kit: a repo with images is a service, and a service runs on kit.
func (e *env) kit() result {
	if len(e.c.Images) == 0 {
		return result{skip: "no images"}
	}
	m, err := e.goMod()
	if err != nil || m == nil {
		return result{} // reported by gomod
	}
	if m.kit == "" {
		return result{findings: []finding{{what: "a service (images) requires " + KitModule, file: e.goPath("go.mod")}}}
	}
	return result{}
}

// The hand-rolled code kit replaces. Built from parts: ship's own source must not match its own rule.
var (
	healthRoute = regexp.MustCompile(`"(?:[A-Z]+ )?/(?:api/(?:v1/)?)?` + `health"`)
	signalWait  = regexp.MustCompile(`signal\.` + `Notify(?:Context)?\(`)
)

// handrolled: version stamping, health routes and shutdown signals come from kit, never a copy.
func (e *env) handrolled() result {
	var fs []finding
	var versionDirs []string
	listed := e.listed()
	var files []string
	for f := range listed {
		if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") && e.inModule(f, listed) {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	for _, f := range files {
		if d := path.Dir(f); (d == "internal/version" || strings.HasSuffix(d, "/internal/version")) &&
			!slices.Contains(versionDirs, d) {
			versionDirs = append(versionDirs, d)
			fs = append(fs, finding{what: "own version package: use " + KitModule + "/version", file: d})
		}
		b, err := e.read(f)
		if err != nil {
			fs = append(fs, finding{what: err.Error(), file: f})
			continue
		}
		if healthRoute.Match(b) {
			fs = append(fs, finding{what: "own health route: use kit's", file: f})
		}
		if signalWait.Match(b) {
			fs = append(fs, finding{what: "own shutdown signal handling: use kit's", file: f})
		}
	}
	return result{findings: fs}
}

// inModule: f belongs to the go.dir module, not to a nested one (kit in ship's repo) or a dir the go tool skips.
func (e *env) inModule(f string, listed map[string]bool) bool {
	rel := f
	if dir := path.Clean(filepath.ToSlash(e.c.Go.Dir)); dir != "." {
		var ok bool
		if rel, ok = strings.CutPrefix(f, dir+"/"); !ok {
			return false
		}
	}
	parts := strings.Split(rel, "/")
	for i, d := range parts[:len(parts)-1] {
		if d == "testdata" || d == "vendor" || strings.HasPrefix(d, "_") || strings.HasPrefix(d, ".") {
			return false
		}
		if listed[e.goPath(strings.Join(parts[:i+1], "/")+"/go.mod")] {
			return false
		}
	}
	return true
}

// agents: AGENTS.md tells every agent the subject style the gate enforces.
func (e *env) agents() result {
	b, err := e.read("AGENTS.md")
	switch {
	case err != nil:
		return result{findings: []finding{{what: err.Error(), file: "AGENTS.md"}}}
	case b == nil:
		return result{findings: []finding{{what: "missing", file: "AGENTS.md"}}}
	case !bytes.Contains(b, []byte(AgentsLine)):
		return result{findings: []finding{{what: "no commit-subject line (README, Adopting ship: `- " + AgentsLine +
			" (…), gated by ship; …`)", file: "AGENTS.md"}}}
	}
	return result{}
}
