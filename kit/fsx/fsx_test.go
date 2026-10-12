package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := WriteFileAtomic(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("two"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "two" {
		t.Fatalf("content = %q, %v", got, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o640 {
		t.Errorf("perm = %v", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestWriteFileAtomicFailureLeavesTarget(t *testing.T) {
	if err := WriteFileAtomic(filepath.Join(t.TempDir(), "missing", "x"), []byte("x"), 0o600); err == nil {
		t.Error("write into a missing directory = nil")
	}
	// A directory in the target's place makes the rename fail after the temp file exists.
	dir := t.TempDir()
	target := filepath.Join(dir, "taken")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(target, []byte("x"), 0o600); err == nil {
		t.Error("rename over a non-empty directory = nil")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}
