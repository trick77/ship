package main

import (
	"os/exec"

	"github.com/trick77/ship/internal/gitx"
)

func gitInit(dir string) ([]byte, error) {
	cmd := exec.Command("git", "init", "-q", "-b", "master")
	cmd.Dir = dir
	cmd.Env = gitx.CleanEnv()
	return cmd.CombinedOutput()
}
