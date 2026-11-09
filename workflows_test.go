package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/trick77/ship/internal/config"
)

// ship-release's dir choices and CI's release matrix cannot be computed (workflow_dispatch options and a matrix
// without an extra job are literals): both follow ship.yaml's go.modules.
func TestShipRelease_whenDispatched_thenEveryModuleOffered(t *testing.T) {
	c, err := config.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(".github/workflows/ship-release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		On struct {
			WorkflowDispatch struct {
				Inputs struct {
					Dir struct {
						Options []string `yaml:"options"`
					} `yaml:"dir"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	// config cleans go.modules: options equal to the cleaned list reach release -dir clean (kit/auth).
	want := append([]string{"ship"}, c.Go.Modules...)
	if got := w.On.WorkflowDispatch.Inputs.Dir.Options; !slices.Equal(got, want) {
		t.Errorf("ship-release dir options %v, want %v (ship + ship.yaml go.modules)", got, want)
	}
}

// CI releases on master each module ship.yaml lists, ship itself as "": a module missing here is never tagged.
func TestCIRelease_whenMaster_thenEveryModuleReleased(t *testing.T) {
	c, err := config.Load(".")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(".github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Jobs struct {
			Release struct {
				Strategy struct {
					Matrix struct {
						Dir []string `yaml:"dir"`
					} `yaml:"matrix"`
				} `yaml:"strategy"`
			} `yaml:"release"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	want := append([]string{""}, c.Go.Modules...)
	if got := w.Jobs.Release.Strategy.Matrix.Dir; !slices.Equal(got, want) {
		t.Errorf("ci.yaml release matrix dir %q, want %q (\"\" + ship.yaml go.modules)", got, want)
	}
}

// A release gates the commit it tags: check.yaml runs in a read-only job the release job needs, on by default; only
// ship's CI, whose check job just gated the same commit, turns it off. A release not waiting for the gate, releasing
// on its failure, or a gate off by default tags what no gate saw as merged (loom 0.10.2). The gate runs the repo's
// code: write permissions there hand them to its npm install scripts.
func TestRelease_whenRun_thenGatedBeforeTagging(t *testing.T) {
	type step struct {
		ID  string            `yaml:"id"`
		If  string            `yaml:"if"`
		Run string            `yaml:"run"`
		Env map[string]string `yaml:"env"`
	}
	type job struct {
		Needs       []string          `yaml:"needs"`
		If          string            `yaml:"if"`
		Uses        string            `yaml:"uses"`
		With        map[string]any    `yaml:"with"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []step            `yaml:"steps"`
	}
	read := func(f string, v any) {
		t.Helper()
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	var w struct {
		On struct {
			WorkflowCall struct {
				Inputs map[string]struct {
					Default any `yaml:"default"`
				} `yaml:"inputs"`
			} `yaml:"workflow_call"`
		} `yaml:"on"`
		Jobs map[string]job `yaml:"jobs"`
	}
	read(".github/workflows/release.yaml", &w)
	if d := w.On.WorkflowCall.Inputs["gate"].Default; d != true {
		t.Errorf("input gate defaults to %v, want true", d)
	}
	resolve, gate, release := w.Jobs["resolve"], w.Jobs["gate"], w.Jobs["release"]
	if gate.Uses != "trick77/ship/.github/workflows/check.yaml@master" || gate.If != "inputs.gate" ||
		!slices.Equal(gate.Needs, []string{"resolve"}) || gate.With["ship"] != "${{ needs.resolve.outputs.ship }}" {
		t.Errorf("gate job: %+v; want check.yaml after resolve, if inputs.gate, the resolved ship", gate)
	}
	for name, j := range map[string]job{"resolve": resolve, "gate": gate} {
		if len(j.Permissions) != 1 || j.Permissions["contents"] != "read" {
			t.Errorf("%s job permissions %v, want contents: read alone", name, j.Permissions)
		}
	}
	if !slices.Contains(release.Needs, "gate") || !slices.Contains(release.Needs, "resolve") ||
		!strings.Contains(release.If, "needs.gate.result == 'success'") ||
		!strings.Contains(release.If, "(needs.gate.result == 'skipped' && !inputs.gate)") {
		t.Errorf("release job needs %v, if %q; want it after a passed gate, or a skipped one with gate off",
			release.Needs, release.If)
	}
	i := slices.IndexFunc(release.Steps, func(s step) bool { return strings.Contains(s.Run, `$SHIP release "$BUMP"`) })
	if i < 0 || release.Steps[i].Env["SHIP"] != "${{ needs.resolve.outputs.ship }}" {
		t.Errorf("release step %d does not run the resolved ship", i)
	}

	// ship's CI turns the gate off (its check job gated the commit); the manual fallback keeps it.
	var ci, manual struct {
		Jobs map[string]job `yaml:"jobs"`
	}
	read(".github/workflows/ci.yaml", &ci)
	read(".github/workflows/ship-release.yaml", &manual)
	if r := ci.Jobs["release"]; !slices.Contains(r.Needs, "check") || r.With["gate"] != false {
		t.Errorf("ci.yaml release: needs %v, gate %v; want needs check and gate false", r.Needs, r.With["gate"])
	}
	if g, ok := manual.Jobs["release"].With["gate"]; ok {
		t.Errorf("ship-release.yaml sets gate %v, want it left on", g)
	}

	// The gate's Node: the input, else the caller's ci.yaml's node-version, else none; a UI without one fails.
	node := slices.IndexFunc(resolve.Steps, func(s step) bool { return s.ID == "node" })
	if node < 0 || resolve.Steps[node].If != "inputs.gate" {
		t.Fatal("no step id node in resolve, or not only with the gate")
	}
	for _, tc := range []struct{ input, ci, shipYAML, want string }{
		{"", "    with:\n      go-dir: backend\n      node-version: \"26\"\n", "ui:\n", "node=26\nui-dir=ui\n"},
		{"", "    with:\n      node-version: '22'\n      ui-dir: web\n", "", "node=22\nui-dir=web\n"},
		{"24", "      node-version: \"26\"\n", "", "node=24\nui-dir=ui\n"},
		{"", "    with:\n      go-dir: backend\n", "go:\n  dir: backend\n", "node=\nui-dir=ui\n"},
		// No ci.yaml; then a UI and no Node found (flow style): fails.
		{"", "", "", "node=\nui-dir=ui\n"},
		{"", "    with: { node-version: \"22\" }\n", "ui:\n  dir: ui\n", ""},
	} {
		dir := t.TempDir()
		ciPath, shipPath, out := filepath.Join(dir, "ci.yaml"), filepath.Join(dir, "ship.yaml"), filepath.Join(dir, "out")
		for p, s := range map[string]string{ciPath: tc.ci, shipPath: tc.shipYAML} {
			if s != "" {
				if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		cmd := exec.Command("bash", "-e", "-c", resolve.Steps[node].Run)
		cmd.Env = append(os.Environ(), "NODE="+tc.input, "CI_FILE="+ciPath, "SHIP_YAML="+shipPath, "GITHUB_OUTPUT="+out)
		msg, err := cmd.CombinedOutput()
		if tc.want == "" {
			if err == nil || !strings.HasPrefix(string(msg), "::error::") {
				t.Errorf("ci %q: want an ::error:: and a failure, got %v %s", tc.ci, err, msg)
			}
			continue
		}
		if got, _ := os.ReadFile(out); err != nil || string(got) != tc.want {
			t.Errorf("input %q, ci %q: got %q (%v %s), want %q", tc.input, tc.ci, got, err, msg, tc.want)
		}
	}
}

// The dependabot job merges to master with no human: only after the gate, only Dependabot's own pushes, only the head
// and the master the gate checked. A broader if or a lost guard merges a human's or a fork's PR, or an unchecked tree.
func TestCIDependabot_whenAutoMerge_thenGatedAndGuarded(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/ci.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Jobs struct {
			Dependabot struct {
				Needs []string `yaml:"needs"`
				If    string   `yaml:"if"`
				Steps []struct {
					Run string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"dependabot"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	j := w.Jobs.Dependabot
	if !slices.Equal(j.Needs, []string{"check"}) {
		t.Errorf("needs %v, want [check]", j.Needs)
	}
	for _, want := range []string{"github.event_name == 'pull_request'", "github.actor == 'dependabot[bot]'",
		"github.event.pull_request.user.login == 'dependabot[bot]'"} {
		if !strings.Contains(j.If, want+" ") && !strings.HasSuffix(j.If, want) {
			t.Errorf("if %q lacks %s", j.If, want)
		}
	}
	if strings.Contains(j.If, "||") {
		t.Errorf("if %q: an || widens it", j.If)
	}
	var run string
	for _, s := range j.Steps {
		run += s.Run + "\n"
	}
	for _, want := range []string{`--match-head-commit "$SHA"`, `commits/$GITHUB_SHA" --jq '.parents[0].sha'`,
		`[ "$gated" != "$master" ]`, "@dependabot rebase"} {
		if !strings.Contains(run, want) {
			t.Errorf("steps lack %s", want)
		}
	}
	if m, g := strings.Index(run, "gh pr merge"), strings.Index(run, `[ "$gated" != "$master" ]`); g < 0 || m < g {
		t.Error("gh pr merge runs before the master guard")
	}
}

// CI's Go is go.mod's major.minor at its newest patch, never setup-go's exact go.mod version: a pinned 1.x.0 misses the
// stdlib fixes govulncheck demands. check.yaml, release.yaml and vuln.yaml each carry the step; this keeps them one.
// Each workflow's step is run as the runner does (bash -e) on go.mod shapes: the higher of go and toolchain lines.
func TestSetupGo_whenRun_thenNewestPatchOfGoMod(t *testing.T) {
	type step struct {
		ID   string            `yaml:"id"`
		Uses string            `yaml:"uses"`
		Run  string            `yaml:"run"`
		Env  map[string]string `yaml:"env"`
		With map[string]string `yaml:"with"`
	}
	var scripts []string
	files := []string{".github/workflows/check.yaml", ".github/workflows/release.yaml", ".github/workflows/vuln.yaml"}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var w struct {
			Jobs map[string]struct {
				Steps []step `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, j := range w.Jobs {
			for i, s := range j.Steps {
				if s.ID != "go" {
					continue
				}
				found = true
				if s.Env["MOD"] != "${{ inputs.go-dir }}/go.mod" {
					t.Errorf("%s: step go reads MOD %q, want go-dir's go.mod", f, s.Env["MOD"])
				}
				scripts = append(scripts, s.Run)
				next := step{}
				if i+1 < len(j.Steps) {
					next = j.Steps[i+1]
				}
				if next.Uses != "actions/setup-go@v7" || next.With["go-version"] != "${{ steps.go.outputs.version }}" ||
					next.With["check-latest"] != "true" || next.With["go-version-file"] != "" {
					t.Errorf("%s: step go not followed by setup-go with its version and check-latest: %+v", f, next)
				}
			}
		}
		if !found {
			t.Fatalf("%s: no step id go", f)
		}
	}
	for i := 1; i < len(scripts); i++ {
		if scripts[i] != scripts[0] {
			t.Errorf("step go differs:\n%s:\n%s\n%s:\n%s", files[0], scripts[0], files[i], scripts[i])
		}
	}
	for _, tc := range []struct{ mod, want string }{
		{"module x\n\ngo 1.27.0\n", "1.27"},
		{"module x\n\ngo 1.27.0 // why\r\n\ntoolchain go1.28.1\n", "1.28"},
		{"module x\n\ngo 1.28.0\n\ntoolchain go1.27.5\n", "1.28"},
		{"module x\n\ngo 1.9.0\n\ntoolchain go1.10.1\n", "1.10"},
		{"module x\n\ngo 1.27.3\n\ntoolchain default\n", "1.27"},
		{"module x\n\ngo 1.27\n", "1.27"},
		{"module x\n", ""},
		{"", ""}, // no go.mod at all
	} {
		dir := t.TempDir()
		mod, out := filepath.Join(dir, "go.mod"), filepath.Join(dir, "out")
		if tc.mod != "" {
			if err := os.WriteFile(mod, []byte(tc.mod), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("bash", "-e", "-c", scripts[0])
		cmd.Env = append(os.Environ(), "MOD="+mod, "GITHUB_OUTPUT="+out)
		msg, err := cmd.CombinedOutput()
		got, _ := os.ReadFile(out)
		if tc.want == "" {
			if err == nil || !strings.HasPrefix(string(msg), "::error::") {
				t.Errorf("%q: want an ::error:: and a failure, got %v %s", tc.mod, err, msg)
			}
			continue
		}
		if err != nil || string(got) != "version="+tc.want+"\n" {
			t.Errorf("%q: got %q (%v %s), want version=%s", tc.mod, got, err, msg, tc.want)
		}
	}
}

// resolve is how everything that runs master's ship finds its sha, before any ship runs: exactly refs/heads/master
// (ls-remote matches by suffix), a 40-hex sha or a stop with the error. Each copy is shell of its own (a workflow
// step, the hook, a Makefile, README's function), none can call another; this keeps them one.
const (
	resolve = `git ls-remote https://github.com/trick77/ship refs/heads/master | awk '$2 == "refs/heads/master" { print $1 }'`
	// The guard: bash in the workflows, POSIX sh elsewhere.
	hexBash = `[[ "$sha" =~ ^[0-9a-f]{40}$ ]] ||`
	hexSh   = `| grep -Eqx '[0-9a-f]{40}' ||`
	fail    = `cannot resolve ship's master on GitHub: '$sha'`
)

func TestMasterResolver_whenCopied_thenSame(t *testing.T) {
	workflow := `sha="$(` + resolve + `)" ` + hexBash + ` { echo "::error::` + fail + `"; exit 1; } ` +
		`SHIP="go run github.com/trick77/ship@$sha"`
	for f, want := range map[string]string{
		".github/workflows/check.yaml":   workflow,
		".github/workflows/release.yaml": workflow,
		".github/workflows/vuln.yaml":    workflow,
		"internal/hook/hook.go": `sha="$(` + resolve + `)" printf '%s\n' "$sha" ` + hexSh + ` { echo "pre-push: ` + fail +
			` (git ls-remote, error above)"; exit 1; } printf '%s\n' "$refs" | go run "github.com/trick77/ship@$sha" pre-push "$@"`,
		"internal/scaffold/templates/Makefile": `sha=$(` + resolve + `); echo "$sha" ` + hexSh + ` { echo "` + fail +
			` (git ls-remote, error above)"; exit 1; }; go run github.com/trick77/ship@$sha`,
		"README.md": `sha="$(` + resolve + `)" printf '%s\n' "$sha" ` + hexSh + ` { echo "ship: ` + fail +
			` (git ls-remote, error above)" >&2; return 1; } go run "github.com/trick77/ship@$sha" "$@"`,
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.HasSuffix(f, "Makefile") {
			s = strings.ReplaceAll(s, "$$", "$") // make's $
		}
		// A continued line, indentation and line breaks are layout, not the block.
		s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\\\n", " ")), " ")
		if !strings.Contains(s, want) {
			t.Errorf("%s: no resolver block\n%s", f, want)
		}
	}
}
