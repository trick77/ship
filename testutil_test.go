package main

import "os/exec"

func gitInit(dir string) ([]byte, error) {
	cmd := exec.Command("git", "init", "-q", "-b", "master")
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func gitIn(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}
