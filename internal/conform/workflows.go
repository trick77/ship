package conform

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/trick77/ship/internal/scaffold"
)

// cleanupActions are image cleanups other than ship's caller: two retention policies drift apart.
var cleanupActions = []string{"dataaxiom/ghcr-cleanup-action", "snok/container-retention-policy",
	"actions/delete-package-versions"}

type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Uses string `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
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

// workflows: ship's callers (ci, release, cleanup-images) are owned files, checked byte for byte by scaffold; this
// rule keeps the one retention policy across every workflow.
func (e *env) workflows() result { return result{findings: e.foreignCleanup()} }

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

// visibilityRule: ship.yaml's visibility agrees with GitHub's. What a visibility asks of CI and dependabot (if:,
// grouping, the PR limit) and the image cleanup are rendered into the owned files for it (scaffold).
func (e *env) visibilityRule() result {
	if e.visErr != nil {
		return result{findings: []finding{{what: e.visErr.Error(), file: "GitHub"}}}
	}
	if e.c.Visibility != "" && e.c.Visibility != e.vis {
		return result{findings: []finding{{what: fmt.Sprintf("visibility: %s, GitHub says %s", e.c.Visibility, e.vis),
			file: "ship.yaml"}}, note: e.visNote}
	}
	return result{note: e.visNote}
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

// dependabot: deps-prefixed bumps (`deps: bump …`, a Go-style subject) for every ecosystem the repo has. The owned
// dependabot.yaml has both (scaffold); this holds where a repo keeps its own (an allowed scaffold diff: ship's).
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
