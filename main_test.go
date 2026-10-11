package main

import (
	"os"
	"strings"
	"testing"
)

func TestRun_whenNoOrUnknownCommand_thenError(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("no command accepted")
	}
	if err := run([]string{"frobnicate"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err = %v", err)
	}
	if err := run([]string{"help"}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_whenNoShipYaml_thenCheckAndReleaseRefuse(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := run([]string{"check"}); err == nil {
		t.Fatal("check outside a repo")
	}
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, args := range [][]string{{"check"}, {"release", "minor", "-dry-run"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "ship.yaml") {
			t.Errorf("%v: %v", args, err)
		}
	}
	if err := run([]string{"release", "-bogus"}); err == nil {
		t.Error("bad flag accepted")
	}
}

func TestReleaseOptions(t *testing.T) {
	for _, tc := range []struct {
		args        []string
		bump, cache string
		dry         bool
	}{
		{nil, "patch", "", false},
		{[]string{"minor"}, "minor", "", false},
		{[]string{"-dry-run", "minor"}, "minor", "", true},
		{[]string{"major", "-cache", "gha", "-dry-run"}, "major", "gha", true},
		{[]string{"-cache", "gha", "major"}, "major", "gha", false},
	} {
		o, err := releaseOptions(tc.args)
		if err != nil || o.Bump != tc.bump || o.Cache != tc.cache || o.DryRun != tc.dry {
			t.Errorf("%v: %+v %v", tc.args, o, err)
		}
	}
	for _, args := range [][]string{{"-dry-run", "minor", "major"}, {"minor", "-dry-run", "major"}, {"-bogus"}} {
		if _, err := releaseOptions(args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
}

func TestRun_whenPrePushOnlyDeletes_thenNothingToGate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString("(delete) 0000000000000000000000000000000000000000 refs/heads/x 1111111111111111111111111111111111111111\n")
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if err := run([]string{"pre-push"}); err != nil {
		t.Fatal(err)
	}
}

func TestRun_whenHooks_thenInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := gitInit(dir); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := run([]string{"hooks"}); err != nil {
		t.Fatal(err)
	}
}
