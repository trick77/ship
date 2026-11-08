package schemadoc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorder is a testing.TB that keeps the failure instead of failing the test.
type recorder struct {
	testing.TB
	failed string
}

func (r *recorder) Helper() {}
func (r *recorder) Fatalf(format string, a ...any) {
	r.failed = fmt.Sprintf(format, a...)
}

func TestGoldenMatch(t *testing.T) {
	t.Setenv(UpdateEnv, "")
	path := filepath.Join(t.TempDir(), "schema.md")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{TB: t}
	Golden(r, []byte("a\nb\n"), path)
	if r.failed != "" {
		t.Errorf("equal content failed: %s", r.failed)
	}
}

func TestGoldenStale(t *testing.T) {
	t.Setenv(UpdateEnv, "")
	path := filepath.Join(t.TempDir(), "schema.md")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{TB: t}
	Golden(r, []byte("a\nB\nc\n"), path)
	for _, want := range []string{path, "line 2", UpdateEnv + "=1", RegenerateWhere} {
		if !strings.Contains(r.failed, want) {
			t.Errorf("failure %q lacks %q", r.failed, want)
		}
	}
}

func TestGoldenMissingFile(t *testing.T) {
	t.Setenv(UpdateEnv, "")
	path := filepath.Join(t.TempDir(), "schema.md")
	r := &recorder{TB: t}
	Golden(r, []byte("a\n"), path)
	if !strings.Contains(r.failed, UpdateEnv+"=1") || !strings.Contains(r.failed, RegenerateWhere) {
		t.Errorf("failure %q does not say how to create it", r.failed)
	}
}

func TestGoldenUpdate(t *testing.T) {
	t.Setenv(UpdateEnv, "1")
	path := filepath.Join(t.TempDir(), "docs", "schema.md")
	r := &recorder{TB: t}
	Golden(r, []byte("new\n"), path)
	if r.failed != "" {
		t.Fatalf("update failed: %s", r.failed)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new\n" {
		t.Errorf("file = %q, %v; want the new content", got, err)
	}
}

func TestGoldenUpdateWriteError(t *testing.T) {
	t.Setenv(UpdateEnv, "1")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{TB: t}
	Golden(r, []byte("x"), filepath.Join(file, "schema.md")) // a file where a directory should be
	if r.failed == "" {
		t.Error("write under a file: no failure")
	}
}
