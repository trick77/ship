package check

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/trick77/ship/internal/gitx"
)

// shebangRe matches the shells shellcheck understands, direct or via env: #!/bin/sh, #!/usr/bin/env bash, … zsh
// and fish are not shellcheck's.
var shebangRe = regexp.MustCompile(`^#!\s*(?:\S*/)?(?:env\s+(?:-S\s+)?)?(?:sh|bash|dash|ksh)(?:\s|$)`)

// shellcheck runs shellcheck -x on the repo's shell scripts: *.sh, files with a sh/bash shebang, and ship.yaml's
// shell: list (a script git cannot tell by name or first line). No script, no shellcheck needed.
func (r runner) shellcheck() error {
	files, err := r.shellFiles()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Println("(none)")
		return nil
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		return fmt.Errorf("%d shell script(s) and no shellcheck on PATH: brew install shellcheck, "+
			"apt-get install shellcheck (GitHub's ubuntu runners have it)", len(files))
	}
	return r.run(r.root, nil, "shellcheck", append([]string{"-x"}, files...)...)
}

// shellFiles lists the scripts shellcheck checks, sorted, repo-relative.
func (r runner) shellFiles() ([]string, error) {
	all, err := gitx.Files(r.root)
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range all {
		if strings.HasSuffix(f, ".sh") || shebang(filepath.Join(r.root, f)) {
			files = append(files, f)
		}
	}
	for _, f := range r.c.Shell {
		if f = filepath.ToSlash(filepath.Clean(f)); !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	slices.Sort(files)
	return files, nil
}

// shebang reports whether the regular file p starts with a shell shebang shellcheck reads.
func shebang(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(p) //nolint:gosec // a file git lists in the repo
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 128)
	n, _ := io.ReadFull(f, b)
	line, _, _ := strings.Cut(string(b[:n]), "\n")
	return shebangRe.MatchString(line)
}
