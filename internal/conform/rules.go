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

	"github.com/trick77/ship/internal/compose"
	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/pins"
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
			fs = append(fs, finding{what: "missing (ship init)", file: f.Path, rerender: true})
		case f.Differs():
			fs = append(fs, finding{what: "differs from ship's template (ship init -diff)", file: f.Path, rerender: true})
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

// uiScripts are the UI's package.json scripts, exactly: ship.yaml's ui.lint runs lint, the build is the typecheck.
var uiScripts = []struct{ name, want string }{
	{"build", "tsc -b && vite build"},
	{"lint", "eslint . --max-warnings 0 && npm run format:check"},
	{"format", "prettier --write 'src/**/*.{ts,tsx,css}' '*.ts'"},
	{"format:check", "prettier --check 'src/**/*.{ts,tsx,css}' '*.ts'"},
	{"test", "vitest run"},
}

// uiDevDependencies are the lint, format and typecheck tools every UI declares. Their versions are the repo's
// (Dependabot moves them per repo), only exact: .npmrc's save-exact, one version per clone.
var uiDevDependencies = []string{"typescript", "prettier", "eslint", "typescript-eslint", "eslint-config-prettier",
	"@eslint/js", "eslint-plugin-react-hooks"}

// exactVersion is an npm version with no range: x.y.z, a pre-release allowed.
var exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)

// typescriptCap: dependabot's typescript ignore holds a UI below it, this catches a hand bump.
const typescriptCap = pins.TypescriptCap

// SetupImport is the first statement of the UI's src/test/setup.ts: jest-dom's matchers on vitest's expect, before
// anything a repo adds there.
const SetupImport = `import "@testing-library/jest-dom/vitest";`

// viteWants are literals vite.config.ts must contain. Plain string checks, not a parser: it is TypeScript, nothing
// in the standard library parses it and ship's dependencies stay stdlib + yaml. The format script covers '*.ts', so
// Prettier fixes quotes and spacing: the literals are stable, and a miss names the line to write.
var viteWants = []struct{ lit, why string }{
	{`from "vitest/config"`, "defineConfig from vitest/config (types the test key)"},
	{"globals: true", "test.globals: true (Testing Library's cleanup)"},
	{`setupFiles: ["./src/test/setup.ts"]`, `test.setupFiles: ["./src/test/setup.ts"]`},
	{`reportsDirectory: "../coverage/ui"`, `coverage.reportsDirectory: "../coverage/ui" (ship.yaml ui.lcov: coverage/ui/lcov.info)`},
}

// ui: one lint (ESLint, then Prettier, every tool at one pinned version), the build is the typecheck, vitest runs
// the tests with one setup.
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
	for _, s := range uiScripts {
		if got := p.Scripts[s.name]; got != s.want {
			fs = append(fs, finding{what: fmt.Sprintf("script %s %q, want %q", s.name, got, s.want), file: pj})
		}
	}
	for _, d := range uiDevDependencies {
		v, ok := p.DevDependencies[d]
		switch {
		case !ok:
			fs = append(fs, finding{what: "devDependency " + d + " missing", file: pj})
		case !exactVersion.MatchString(v):
			fs = append(fs, finding{what: fmt.Sprintf("devDependency %s %q: want an exact version (.npmrc save-exact)",
				d, v), file: pj})
		case d == "typescript" && atLeast(v, typescriptCap):
			fs = append(fs, finding{what: fmt.Sprintf("devDependency typescript %s: want below %s, typescript-eslint "+
				"does not support it", v, typescriptCap), file: pj})
		}
	}
	if es := e.uiPath(scaffold.ESLint); !e.exists(es) {
		fs = append(fs, finding{what: "missing (ship init)", file: es})
	}
	fs = append(fs, e.tsconfigRef()...)
	fs = append(fs, e.viteConfig()...)
	fs = append(fs, e.testSetup()...)
	return result{findings: fs}
}

// tsconfigNodeRef is tsconfig.json's project reference to the owned tsconfig.node.json. A pattern, not encoding/json:
// tsconfig is JSONC (transmission-ui's carries comments), and Prettier does not format it, so spacing varies.
var tsconfigNodeRef = regexp.MustCompile(`"path"\s*:\s*"\./` + regexp.QuoteMeta(scaffold.TSConfigNode) + `"`)

// tsconfigRef: tsc -b builds vite.config.ts through the project reference to the owned tsconfig.node.json.
func (e *env) tsconfigRef() []finding {
	f := e.uiPath("tsconfig.json")
	b, err := e.read(f)
	switch {
	case err != nil:
		return []finding{{what: err.Error(), file: f}}
	case b == nil:
		return []finding{{what: "missing", file: f}}
	case !tsconfigNodeRef.Match(b):
		return []finding{{what: fmt.Sprintf(`no reference to ./%s ("references": [{ "path": "./%[1]s" }])`,
			scaffold.TSConfigNode), file: f}}
	}
	return nil
}

// viteConfig: vitest typed, globals, the one setup file, one coverage directory (viteWants).
func (e *env) viteConfig() []finding {
	f := e.uiPath("vite.config.ts")
	b, err := e.read(f)
	switch {
	case err != nil:
		return []finding{{what: err.Error(), file: f}}
	case b == nil:
		return []finding{{what: "missing", file: f}}
	}
	var fs []finding
	for _, w := range viteWants {
		if !bytes.Contains(b, []byte(w.lit)) {
			fs = append(fs, finding{what: fmt.Sprintf("no %s: want %s", w.lit, w.why), file: f})
		}
	}
	return fs
}

// testSetup: setup.ts is the repo's (stubs jsdom lacks go there), its first statement is ship's. A leading comment
// (line or block) is no statement.
func (e *env) testSetup() []finding {
	f := e.uiPath("src/test/setup.ts")
	b, err := e.read(f)
	switch {
	case err != nil:
		return []finding{{what: err.Error(), file: f}}
	case b == nil:
		return []finding{{what: "missing (first statement " + SetupImport + ")", file: f}}
	}
	s := string(b)
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "//"):
			_, s, _ = strings.Cut(s, "\n")
			continue
		case strings.HasPrefix(s, "/*"):
			_, s, _ = strings.Cut(s[2:], "*/")
			continue
		}
		break
	}
	if first, _, _ := strings.Cut(s, "\n"); strings.TrimSpace(first) == SetupImport {
		return nil
	}
	return []finding{{what: "first statement is not " + SetupImport, file: f}}
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

func (e *env) goMod() (*goMod, error) { return e.goModAt(e.goPath("go.mod")) }

// goModAt reads the go.mod at file, nil when it does not exist.
func (e *env) goModAt(file string) (*goMod, error) {
	b, err := e.read(file)
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

// goLineFindings: go x.y.z, x.y at least pins.GoMin.
func goLineFindings(file, v string) []finding {
	if !fullGo.MatchString(v) {
		return []finding{{what: fmt.Sprintf("go %s: want x.y.z", v), file: file}}
	}
	if !atLeast(v, pins.GoMin) {
		return []finding{{what: fmt.Sprintf("go %s: want at least go %s.0 (pins.GoMin)", v, pins.GoMin), file: file}}
	}
	return nil
}

// atLeast compares dotted versions over floor's parts: "1.27.3" is at least "1.27", "1.26.9" is not.
func atLeast(v, floor string) bool {
	vs, ms := strings.Split(v, "."), strings.Split(floor, ".")
	for i, mp := range ms {
		m, _ := strconv.Atoi(mp)
		x := 0
		if i < len(vs) {
			x, _ = strconv.Atoi(vs[i])
		}
		if x != m {
			return x > m
		}
	}
	return true
}

// gomod: every module's go line is a full toolchain version (go mod tidy's own form) at least pins.GoMin. kit's
// version is Dependabot's to move like any dependency's: a kit release never fails a consumer's push.
func (e *env) gomod() result {
	file := e.goPath("go.mod")
	m, err := e.goMod()
	switch {
	case err != nil:
		return result{findings: []finding{{what: err.Error(), file: file}}}
	case m == nil:
		return result{findings: []finding{{what: "missing", file: file}}}
	}
	fs := goLineFindings(file, m.goVersion)
	for _, dir := range e.c.Go.Modules {
		f := path.Join(filepath.ToSlash(dir), "go.mod")
		switch nm, err := e.goModAt(f); {
		case err != nil:
			fs = append(fs, finding{what: err.Error(), file: f})
		case nm == nil:
			fs = append(fs, finding{what: "missing (ship.yaml go.modules)", file: f})
		default:
			fs = append(fs, goLineFindings(f, nm.goVersion)...)
		}
	}
	return result{findings: fs}
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
	if m.kit != "" {
		return result{}
	}
	return result{findings: []finding{{what: "a service (images) requires " + KitModule, file: e.goPath("go.mod")}}}
}

// The hand-rolled code kit replaces. Built from parts: ship's own source must not match its own rule.
var (
	// A probe path as a route: a ServeMux method pattern (blanks or a \t escape after the method, an optional host)
	// or a Handle/HandleFunc argument, quoted or raw. A path named as a URL is no route: kit's own probe,
	// httpx.Healthcheck(httpx.LocalURL(addr, "/healthz")), names one.
	healthRoute = regexp.MustCompile(`(?:["` + "`" + `][A-Z]+(?:[ \t]|\\t)+[^"` + "`" + `/ \t\\]*|Handle(?:Func)?\(\s*["` +
		"`" + `])/(?:api/(?:v1/)?)?` + `(?:healthz?|readyz)["` + "`]")
	signalWait = regexp.MustCompile(`signal\.` + `Notify(?:Context)?\(`)
	oidcImport = regexp.MustCompile(`"github\.com/coreos/` + `go-oidc[/"]`)
)

// handrolled: version stamping, health routes, shutdown signals and OIDC sign-in come from kit, never a copy.
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
			fs = append(fs, finding{what: "own shutdown signal handling: use kit's (run.Context, httpx.Serve)", file: f})
		}
		if oidcImport.Match(b) {
			fs = append(fs, finding{what: "own OIDC: use " + KitModule + "/auth", file: f})
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

// postgres: the test DB image is pinned once. compose:<service> must resolve; a literal image (or version) of a
// repository a root compose service names too is a second pin Dependabot never bumps, drifting from the first.
func (e *env) postgres() result {
	if e.c.Postgres == "" {
		return result{skip: "no postgres"}
	}
	files, svcs, err := e.composeServices()
	if err != nil {
		return result{findings: []finding{{what: err.Error(), file: config.File}}}
	}
	if svc, ok := e.c.PostgresCompose(); ok {
		img, err := compose.Resolve(svc, files, svcs)
		if err != nil {
			return result{findings: []finding{{what: err.Error(), file: config.File}}}
		}
		return result{note: img}
	}
	want := compose.Repository(e.c.PostgresImage())
	var fs []finding
	seen := map[string]bool{}
	for _, s := range svcs {
		if lit := s.Literal(); !seen[s.Name] && lit != "" && compose.Repository(lit) == want {
			seen[s.Name] = true
			fs = append(fs, finding{what: fmt.Sprintf("duplicates compose service %s's image, use compose:%s",
				s.Name, s.Name), file: config.File})
		}
	}
	return result{findings: fs}
}

// composeServices: the root compose files present on disk (e.listed, a tracked file deleted skipped) and their
// services.
func (e *env) composeServices() ([]string, []compose.Service, error) {
	var files []string
	var all []compose.Service
	for _, f := range compose.Files(e.listed()) {
		b, err := e.read(f)
		if err != nil {
			return nil, nil, err
		}
		if b == nil {
			continue
		}
		svcs, err := compose.Parse(f, b)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, f)
		all = append(all, svcs...)
	}
	return files, all, nil
}
