package schemadoc

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// UpdateEnv set to anything non-empty makes Golden write the file instead of comparing. An environment variable,
// not a test flag: `go test ./... -update` hands the flag to every package's test binary, and each one that does not
// link this package exits with "flag provided but not defined".
const UpdateEnv = "SCHEMADOC_UPDATE"

// Regenerate is the command that rewrites a consumer's doc, by the convention that its test is named TestSchemaDoc.
const Regenerate = UpdateEnv + "=1 go test ./... -run TestSchemaDoc"

// Golden fails t when the file at path does not hold got, naming the first differing line; with UpdateEnv set it
// writes got there instead, creating the directory.
func Golden(t testing.TB, got []byte, path string) {
	t.Helper()
	if os.Getenv(UpdateEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a repo directory
			t.Fatalf("schemadoc: %v", err)
			return
		}
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // a doc committed to the repo
			t.Fatalf("schemadoc: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // the caller's own doc path
	if err != nil {
		t.Fatalf("schemadoc: %v; write it with %s", err, Regenerate)
		return
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("schemadoc: %s is stale, first difference at line %d; regenerate with %s", path, firstDiff(want, got), Regenerate)
	}
}

// firstDiff is the 1-based number of the first line where a and b differ.
func firstDiff(a, b []byte) int {
	al, bl := bytes.Split(a, []byte("\n")), bytes.Split(b, []byte("\n"))
	for i := range min(len(al), len(bl)) {
		if !bytes.Equal(al[i], bl[i]) {
			return i + 1
		}
	}
	return min(len(al), len(bl)) + 1
}
