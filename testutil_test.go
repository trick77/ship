package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

func gitInit(dir string) ([]byte, error) { return gitIn(dir, "init", "-q", "-b", "master") }

// gitIn runs git in dir without the parent's repo variables: under a hook they would point it at the real repo.
func gitIn(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !slices.Contains(gitx.RepoVars, k) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd.CombinedOutput()
}
