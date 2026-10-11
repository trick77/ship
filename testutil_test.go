package main

import "os/exec"

func gitInit(dir string) ([]byte, error) {
	cmd := exec.Command("git", "init", "-q", "-b", "master")
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
