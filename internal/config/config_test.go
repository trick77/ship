package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_whenEmpty_thenFamilyDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Base != "master" || c.Lint != "v2.14.0" || c.Patch != 75 || c.Go.Dir != "." || c.Go.Floor != 75 {
		t.Fatalf("defaults: %+v", c)
	}
	if !c.Go.RaceOn() || len(c.Go.Exclude) != 1 || c.Go.Exclude[0] != "cmd/" || c.UI != nil {
		t.Fatalf("go defaults: %+v", c.Go)
	}
}

func TestParse_whenUIAndImages_thenTheirDefaults(t *testing.T) {
	c, err := Parse([]byte("ui: {}\nimages:\n  - suffix: -dashboard\n    target: dashboard\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.Dir != "ui" || c.UI.Floor != 75 || c.UI.Lcov != filepath.Join("ui", "coverage", "lcov.info") ||
		!strings.Contains(c.UI.Build, "npm ci") || !strings.Contains(c.UI.Test, "--coverage") {
		t.Fatalf("ui: %+v", c.UI)
	}
	im := c.Images[0]
	if im.Containerfile != "Containerfile" || im.Context != "." || im.Platforms[0] != "linux/amd64" || im.Target != "dashboard" {
		t.Fatalf("image: %+v", im)
	}
}

func TestParse_whenRaceOff_thenOff(t *testing.T) {
	c, err := Parse([]byte("go:\n  race: false\n  exclude: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Go.RaceOn() || len(c.Go.Exclude) != 0 {
		t.Fatalf("%+v", c.Go)
	}
}

func TestParse_whenUnknownKeyOrBadValue_thenError(t *testing.T) {
	for _, in := range []string{
		"pach: 80\n",            // typo must not silently drop a gate
		"patch: 101\n",          // out of range
		"go:\n  floor: -1\n",    // out of range
		"ui:\n  floor: 200\n",   // out of range
		"steps:\n  - name: x\n", // no run
		"go: [1, 2]\n",          // wrong type
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

func TestLoad_whenMissing_thenError(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), File) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoad_whenPresent_thenParsed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), []byte("patch: 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil || c.Patch != 80 {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}
