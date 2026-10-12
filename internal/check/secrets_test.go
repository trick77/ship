package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/gitx"
)

// key builds a key-shaped string at run time: a literal one would trip ship's own scan.
func key(prefix string) string { return prefix + strings.Repeat("Ab3", 8) }

func TestSecretIn(t *testing.T) {
	for line, hit := range map[string]bool{
		"api_key = \"" + key("sk-") + "\"":              true,
		"token: " + key("tp-"):                          true,
		key("ghp_") + strings.Repeat("x", 12):           true,
		"AKIA" + strings.Repeat("Q", 16):                true,
		"use an sk- key, see docs":                      false,
		"http-" + strings.Repeat("handler-", 4):         false, // no word boundary before tp-
		"disk-" + strings.Repeat("encryption-", 3):      false,
		"k := \"" + key("sk-") + "\" // " + AllowSecret: false,
	} {
		if got := secretIn(line) != ""; got != hit {
			t.Errorf("%q: hit %v", line, got)
		}
	}
	if m := secretIn(key("sk-")); m != "sk-Ab3…" {
		t.Errorf("not redacted: %q", m)
	}
}

func TestSecrets_whenClean_thenPass(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("KEY=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin.dat"), []byte("\x00"+key("sk-")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("lib", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := (runner{root: dir, c: c}).secrets(); err != nil {
		t.Fatal(err)
	}
}

func TestSecrets_whenKeyInTree_thenFail(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x\nkey "+key("sk-")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := (runner{root: dir, c: c}).secrets()
	if err == nil || !strings.Contains(err.Error(), "notes.md:2: sk-Ab3…") || strings.Contains(err.Error(), key("sk-")) {
		t.Fatalf("err = %v", err)
	}
}

func TestSecrets_whenEnvTracked_thenFail(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	if err := os.WriteFile(filepath.Join(dir, "lib", ".env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (runner{root: dir, c: c}).secrets(); err == nil || !strings.Contains(err.Error(), "lib/.env: .env in the tree") {
		t.Fatalf("err = %v", err)
	}
}

// A key added in one commit and removed in the next is clean at the tip, but the push still carries it.
func TestSecrets_whenKeyOnlyInHistory_thenFail(t *testing.T) {
	dir, c := fixture(t, "", "mode: set\n", "")
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "lib", "k.go")
	if err := os.WriteFile(p, []byte("package lib\n\nconst k = \""+key("sk-")+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "lib: k\n\nupstream said: "+key("tp-"))
	git("rm", "-q", "lib/k.go", ".env")
	git("commit", "-q", "-m", "lib: drop k")
	err := (runner{root: dir, c: c}).secrets()
	if err == nil {
		t.Fatal("key in history passed")
	}
	for _, want := range []string{"lib/k.go: sk-Ab3…", "commit message:3: tp-Ab3…", ".env: a commit not yet on origin adds .env"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestScanPatch_whenAddedLineLooksLikeHeader_thenScanned(t *testing.T) {
	patch := "commit abc\n--- a/x\n+++ b/x\n@@ -1,0 +1,2 @@\n+++ " + key("sk-") + "\n+ok\n"
	hits := scanPatch(patch)
	if len(hits) != 1 || !strings.HasPrefix(hits[0], "commit abc x: sk-") {
		t.Fatalf("hits: %v", hits)
	}
}
