package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/pins"
)

func TestPrettier(t *testing.T) {
	dir, c := fixture(t, "ui: {}\n", "mode: set\n", "")
	r := runner{root: dir, c: c}
	pkg := filepath.Join(dir, "ui", "package.json")
	for body, want := range map[string]string{
		`{"devDependencies": {"prettier": "` + pins.Prettier + `"}}`:  "",
		`{"devDependencies": {"prettier": "^` + pins.Prettier + `"}}`: "prettier ^" + pins.Prettier + ", ship pins " + pins.Prettier,
		`{"dependencies": {}}`: "prettier (none), ship pins",
		`{`:                    "package.json",
	} {
		if err := os.WriteFile(pkg, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		err := r.prettier()
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: err = %v", body, err)
		}
	}
	if err := os.Remove(pkg); err != nil {
		t.Fatal(err)
	}
	if err := r.prettier(); err == nil {
		t.Fatal("missing package.json passed")
	}
	r.c.UI = nil
	if err := r.prettier(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanEmbed(t *testing.T) {
	dir, c := fixture(t, "ui:\n  embed: web/dist\n", "mode: set\n", "")
	r := runner{root: dir, c: c}
	dist := filepath.Join(dir, "web", "dist")
	if err := r.cleanEmbed(); err != nil {
		t.Fatalf("missing dir: %v", err)
	}
	for _, p := range []string{".gitkeep", "index.html", "assets/a.js"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dist, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dist, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.Git(dir, "add", "web/dist/.gitkeep"); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanEmbed(); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Name() != ".gitkeep" {
		t.Fatalf("left: %v", left)
	}

	if err := os.WriteFile(filepath.Join(dist, "keep.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(dir, "add", "web/dist/keep.go"); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanEmbed(); err == nil || !strings.Contains(err.Error(), "tracked files") {
		t.Fatalf("tracked file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dist, "keep.go")); err != nil {
		t.Fatal("tracked file deleted")
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	for embed, want := range map[string]string{".": "not a directory inside", "out": "not a directory inside",
		"ui": "holds sources", "../x": "not a directory inside"} {
		r.c.UI = &config.UI{Dir: "ui", Embed: embed}
		if err := r.cleanEmbed(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", embed, err)
		}
	}
	r.c.UI = nil
	if err := r.cleanEmbed(); err != nil {
		t.Fatal(err)
	}
}
