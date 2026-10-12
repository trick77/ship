package check

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestShebangRe(t *testing.T) {
	for line, want := range map[string]bool{
		"#!/bin/sh":                    true,
		"#!/bin/bash -e":               true,
		"#!/usr/bin/env bash":          true,
		"#! /usr/bin/env -S bash -eu":  true,
		"#!/bin/dash":                  true,
		"#!/usr/bin/env zsh":           false,
		"#!/usr/bin/env python3":       false,
		"#!/bin/shell-thing":           false,
		"# just a comment with sh in":  false,
		"package main // #!/bin/sh no": false,
	} {
		if got := shebangRe.MatchString(line); got != want {
			t.Errorf("%q: %v", line, got)
		}
	}
}

func TestShellcheck(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	r := runner{root: dir, c: c}
	log := fakeBin(t, "shellcheck", "exit 0")
	if err := r.shellcheck(); err != nil {
		t.Fatal(err)
	}
	if args := readLog(t, log); args != "" {
		t.Fatalf("shellcheck ran without scripts: %q", args)
	}
	for p, s := range map[string]string{
		"scripts/a.sh": "echo a\n",
		"bin/run":      "#!/usr/bin/env bash\necho run\n",
		"bin/tool.py":  "#!/usr/bin/env python3\n",
		"hooks/x":      "echo x\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c.Shell = []string{"hooks/x", "./scripts/a.sh"}
	if err := r.shellcheck(); err != nil {
		t.Fatal(err)
	}
	if args := readLog(t, log); args != "-x bin/run hooks/x scripts/a.sh\n" {
		t.Fatalf("args: %q", args)
	}
	fakeBin(t, "shellcheck", "exit 1")
	if err := r.shellcheck(); err == nil {
		t.Fatal("shellcheck finding passed")
	}
	// PATH with git alone.
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	only := t.TempDir()
	if err := os.Symlink(git, filepath.Join(only, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", only)
	if err := r.shellcheck(); err == nil || !strings.Contains(err.Error(), "no shellcheck on PATH") {
		t.Fatalf("missing shellcheck: %v", err)
	}
}
