package conform

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/trick77/ship/internal/scaffold"
)

const reusable = "trick77/ship/.github/workflows/"

// cleanupActions are image cleanups other than ship's caller: two retention policies drift apart.
var cleanupActions = []string{"dataaxiom/ghcr-cleanup-action", "snok/container-retention-policy",
	"actions/delete-package-versions"}

type workflow struct {
	On   yaml.Node      `yaml:"on"`
	Jobs map[string]job `yaml:"jobs"`
}

type job struct {
	If          string `yaml:"if"`
	Uses        string `yaml:"uses"`
	Permissions any    `yaml:"permissions"`
	Steps       []struct {
		Uses string `yaml:"uses"`
	} `yaml:"steps"`
}

// loadWorkflow parses a workflow; nil when the file does not exist.
func (e *env) loadWorkflow(p string) (*workflow, error) {
	b, err := e.read(p)
	if err != nil || b == nil {
		return nil, err
	}
	var w workflow
	if err := yaml.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &w, nil
}

// caller finds the job calling ship's reusable workflow name (any ref), sorted by job name for a stable pick.
func (w *workflow) caller(name string) (job, bool) {
	names := make([]string, 0, len(w.Jobs))
	for n := range w.Jobs {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if j := w.Jobs[n]; strings.HasPrefix(j.Uses, reusable+name+"@") {
			return j, true
		}
	}
	return job{}, false
}

// workflows: ship's callers @master, release's grants and auto; no image cleanup but ship's caller, in any workflow.
func (e *env) workflows() result {
	var fs []finding
	callers := []struct{ file, name string }{{scaffold.CI, "check.yaml"}, {scaffold.Release, "release.yaml"},
		{scaffold.Vuln, "vuln.yaml"}}
	if len(e.c.Images) > 0 {
		callers = append(callers, struct{ file, name string }{scaffold.Cleanup, "cleanup-images.yaml"})
	}
	for _, c := range callers {
		w, err := e.loadWorkflow(c.file)
		switch {
		case err != nil:
			fs = append(fs, finding{what: err.Error(), file: c.file})
			continue
		case w == nil:
			fs = append(fs, finding{what: "missing (ship init)", file: c.file})
			continue
		}
		j, ok := w.caller(c.name)
		switch {
		case !ok:
			fs = append(fs, finding{what: "no job calls " + reusable + c.name + "@master", file: c.file})
			continue
		case j.Uses != reusable+c.name+"@master":
			fs = append(fs, finding{what: j.Uses + ": call it @master, as every consumer runs master's ship", file: c.file})
		}
		if c.name == "release.yaml" {
			fs = append(fs, e.releaseCaller(w, j)...)
		}
	}
	fs = append(fs, e.foreignCleanup()...)
	return result{findings: fs}
}

// releaseCaller: packages: write and id-token: write exactly when ship.yaml has images (the release job inherits
// the caller's grant), and auto among the bumps.
func (e *env) releaseCaller(w *workflow, j job) []finding {
	var fs []finding
	perms, _ := j.Permissions.(map[string]any)
	images := len(e.c.Images) > 0
	switch pkgs := perms["packages"] == "write"; {
	case images && !pkgs:
		fs = append(fs, finding{what: "ship.yaml has images: the release job needs permissions packages: write",
			file: scaffold.Release})
	case !images && pkgs:
		fs = append(fs, finding{what: "packages: write without images in ship.yaml: drop it", file: scaffold.Release})
	}
	// cosign signs keyless with the job's OIDC token: without it every image release fails after the push.
	switch oidc := perms["id-token"] == "write"; {
	case images && !oidc:
		fs = append(fs, finding{what: "ship.yaml has images: the release job needs permissions id-token: write (cosign)",
			file: scaffold.Release})
	case !images && oidc:
		fs = append(fs, finding{what: "id-token: write without images in ship.yaml: drop it", file: scaffold.Release})
	}
	var on struct {
		WorkflowDispatch struct {
			Inputs struct {
				Bump struct {
					Options []string `yaml:"options"`
				} `yaml:"bump"`
			} `yaml:"inputs"`
		} `yaml:"workflow_dispatch"`
	}
	_ = w.On.Decode(&on) // a scalar `on:` has no inputs: reported as no auto
	if !slices.Contains(on.WorkflowDispatch.Inputs.Bump.Options, "auto") {
		fs = append(fs, finding{what: "workflow_dispatch bump options lack auto", file: scaffold.Release})
	}
	return fs
}

// foreignCleanup finds image cleanup actions in any workflow: ship's caller is the one retention policy.
func (e *env) foreignCleanup() []finding {
	var fs []finding
	for _, p := range e.workflowFiles() {
		w, err := e.loadWorkflow(p)
		if err != nil {
			fs = append(fs, finding{what: err.Error(), file: p})
			continue
		}
		for _, j := range w.Jobs {
			for _, s := range j.Steps {
				for _, a := range cleanupActions {
					if strings.HasPrefix(s.Uses, a+"@") {
						fs = append(fs, finding{what: "image cleanup by " + a + ": use the cleanup-images caller",
							file: p})
					}
				}
			}
		}
	}
	return fs
}

func (e *env) workflowFiles() []string {
	var ps []string
	for f := range e.listed() {
		if path.Dir(f) == ".github/workflows" && (strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml")) {
			ps = append(ps, f)
		}
	}
	slices.Sort(ps)
	return ps
}

// visibilityRule: ship.yaml's visibility agrees with GitHub's, and CI and dependabot fit it. Public: every PR runs
// CI (forks and Dependabot have no hook). Private: Dependabot PRs only, every npm and docker bump grouped and few
// PRs open (each runs billed CI), no scheduled vuln scan (billed minutes), and old images cleaned up (storage is
// billed).
func (e *env) visibilityRule() result {
	if e.visErr != nil {
		return result{findings: []finding{{what: e.visErr.Error(), file: "GitHub"}}}
	}
	var fs []finding
	if e.c.Visibility != "" && e.c.Visibility != e.vis {
		fs = append(fs, finding{what: fmt.Sprintf("visibility: %s, GitHub says %s", e.c.Visibility, e.vis),
			file: "ship.yaml"})
	}
	w, err := e.loadWorkflow(scaffold.CI)
	if err != nil {
		fs = append(fs, finding{what: err.Error(), file: scaffold.CI})
	} else if w != nil {
		if j, ok := w.caller("check.yaml"); ok {
			switch {
			case e.vis == "public" && j.If != "":
				fs = append(fs, finding{what: "public repo: every PR runs CI, drop the check job's if:", file: scaffold.CI})
			case e.vis == "private" && strings.TrimSpace(j.If) != scaffold.DependabotOnly:
				fs = append(fs, finding{what: "private repo: the check job needs if: " + scaffold.DependabotOnly,
					file: scaffold.CI})
			}
		}
	}
	if e.vis != "private" {
		return result{findings: fs, note: e.visNote}
	}
	d, err := e.loadDependabot()
	if err != nil {
		fs = append(fs, finding{what: err.Error(), file: scaffold.Dependabot})
	} else if d != nil {
		for _, u := range d.Updates {
			if u.Limit != nil && *u.Limit > 5 {
				fs = append(fs, finding{what: fmt.Sprintf("%s %s: open-pull-requests-limit %d, at most 5",
					u.Ecosystem, u.Directory, *u.Limit), file: scaffold.Dependabot})
			}
			if (u.Ecosystem == "npm" || u.Ecosystem == "docker" || u.Ecosystem == "docker-compose") && !u.groupsAll() {
				fs = append(fs, finding{what: fmt.Sprintf("%s %s: not every update grouped (a \"*\" group per "+
					"update type)", u.Ecosystem, u.Directory), file: scaffold.Dependabot})
			}
		}
	}
	if w, err := e.loadWorkflow(scaffold.Vuln); err != nil {
		fs = append(fs, finding{what: err.Error(), file: scaffold.Vuln})
	} else if w != nil {
		var on map[string]yaml.Node
		if w.On.Decode(&on) == nil {
			if _, ok := on["schedule"]; ok {
				fs = append(fs, finding{what: "private repo: no schedule (Actions minutes), workflow_dispatch only",
					file: scaffold.Vuln})
			}
		}
	}
	if len(e.c.Images) > 0 && !e.exists(scaffold.Cleanup) {
		fs = append(fs, finding{what: "private repo with images: the cleanup-images caller is missing (ship init)",
			file: scaffold.Cleanup})
	}
	return result{findings: fs, note: e.visNote}
}

type dependabotFile struct {
	Updates []update `yaml:"updates"`
}

type update struct {
	Ecosystem     string `yaml:"package-ecosystem"`
	Directory     string `yaml:"directory"`
	CommitMessage struct {
		Prefix string `yaml:"prefix"`
	} `yaml:"commit-message"`
	Limit  *int `yaml:"open-pull-requests-limit"`
	Groups map[string]struct {
		Patterns    []string `yaml:"patterns"`
		UpdateTypes []string `yaml:"update-types"`
	} `yaml:"groups"`
}

// groupsAll reports whether "*" groups cover every update type: no bump opens a PR of its own.
func (u update) groupsAll() bool {
	covered := map[string]bool{}
	for _, g := range u.Groups {
		if !slices.Contains(g.Patterns, "*") {
			continue
		}
		if len(g.UpdateTypes) == 0 {
			return true
		}
		for _, t := range g.UpdateTypes {
			covered[t] = true
		}
	}
	return covered["major"] && covered["minor"] && covered["patch"]
}

func (e *env) loadDependabot() (*dependabotFile, error) {
	b, err := e.read(scaffold.Dependabot)
	if err != nil || b == nil {
		return nil, err
	}
	var d dependabotFile
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", scaffold.Dependabot, err)
	}
	return &d, nil
}

// dependabot: deps-prefixed bumps (`deps: bump …`, a Go-style subject) for every ecosystem the repo has.
func (e *env) dependabot() result {
	if e.exists(".github/dependabot.yml") {
		return result{findings: []finding{{what: "rename to dependabot.yaml", file: ".github/dependabot.yml"}}}
	}
	d, err := e.loadDependabot()
	switch {
	case err != nil:
		return result{findings: []finding{{what: err.Error(), file: scaffold.Dependabot}}}
	case d == nil:
		return result{findings: []finding{{what: "missing (ship init)", file: scaffold.Dependabot}}}
	}
	var fs []finding
	for _, u := range d.Updates {
		if u.CommitMessage.Prefix != "deps" {
			fs = append(fs, finding{what: fmt.Sprintf("%s %s: commit-message prefix %q, want deps", u.Ecosystem,
				u.Directory, u.CommitMessage.Prefix), file: scaffold.Dependabot})
		}
	}
	want := []struct{ eco, dir string }{{"gomod", abs(e.c.Go.Dir)}, {"github-actions", "/"}}
	for _, m := range e.c.Go.Modules {
		want = append(want, struct{ eco, dir string }{"gomod", abs(m)})
	}
	if e.c.UI != nil {
		want = append(want, struct{ eco, dir string }{"npm", abs(e.c.UI.Dir)})
	}
	for _, dir := range e.dockerDirs() {
		want = append(want, struct{ eco, dir string }{"docker", dir})
	}
	if e.compose {
		want = append(want, struct{ eco, dir string }{"docker-compose", "/"})
	}
	for _, w := range want {
		if !slices.ContainsFunc(d.Updates, func(u update) bool {
			return u.Ecosystem == w.eco && abs(u.Directory) == w.dir
		}) {
			fs = append(fs, finding{what: fmt.Sprintf("no %s entry for %s", w.eco, w.dir), file: scaffold.Dependabot})
		}
	}
	return result{findings: fs}
}

func abs(dir string) string { return path.Join("/", strings.TrimPrefix(dir, "./")) }
