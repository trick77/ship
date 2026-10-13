package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/trick77/ship/internal/config"
)

// ship-release's dir choices cannot be computed (workflow_dispatch options are literals): they follow ship.yaml's
// go.modules, as CI's release matrix does by reading it.
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
	want := append([]string{"ship"}, c.Go.Modules...)
	if got := w.On.WorkflowDispatch.Inputs.Dir.Options; !slices.Equal(got, want) {
		t.Errorf("ship-release dir options %v, want %v (ship + ship.yaml go.modules)", got, want)
	}
}

// resolve is how everything that runs master's ship finds its sha, before any ship runs: exactly refs/heads/master
// (ls-remote matches by suffix). Each copy is shell of its own (a workflow step, the hook, a Makefile, README's
// function), none can call another; this keeps them one.
const resolve = `git ls-remote https://github.com/trick77/ship refs/heads/master | awk '$2 == "refs/heads/master" { print $1 }'`

func TestMasterResolver_whenCopied_thenSame(t *testing.T) {
	for _, f := range []string{".github/workflows/check.yaml", ".github/workflows/release.yaml",
		"internal/hook/hook.go", "internal/scaffold/templates/Makefile", "README.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.HasSuffix(f, "Makefile") {
			// make's $$ and a continued line.
			s = strings.ReplaceAll(strings.ReplaceAll(s, "$$", "$"), "| \\\n\t\tawk", "| awk")
		}
		if !strings.Contains(s, resolve) {
			t.Errorf("%s: no %s", f, resolve)
		}
	}
}
