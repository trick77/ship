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

// CI's Go is go.mod's major.minor at its newest patch, never setup-go's exact go.mod version: a pinned 1.x.0 misses the
// stdlib fixes govulncheck demands. check.yaml and release.yaml each carry the step; this keeps them one.
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
	for _, f := range []string{".github/workflows/check.yaml", ".github/workflows/release.yaml"} {
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
	if scripts[0] != scripts[1] {
		t.Errorf("step go differs:\ncheck.yaml:\n%s\nrelease.yaml:\n%s", scripts[0], scripts[1])
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
