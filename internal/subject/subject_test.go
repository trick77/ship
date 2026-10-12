package subject

import (
	"slices"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	long := "hook: " + strings.Repeat("x", Max-len("hook: "))
	for _, tc := range []struct {
		s    string
		want string // "" = valid, else part of the error
	}{
		{"hook: pin GIT_DIR before cd", ""},
		{"net/http, net/url: fix parsing", ""},
		{"cmd/go,cmd/link: share flags", ""},
		{"README.md: document the gate", ""},
		{"go/types: Checker reports cycles", ""},
		{"release: build first (#13)", ""},
		{long, ""},
		{long + " (#123)", ""},
		{long + "x", "77 chars"},
		{`Revert "hook: pin GIT_DIR"`, ""},
		{`Revert "pin GIT_DIR"`, "area: what"},
		{"fixup! hook: pin GIT_DIR", ""},
		{"squash! amend! gate: x", ""},
		{"fixup! pin it", "area: what"},
		{"hook: pin GIT_DIR.", "period"},
		{"fix(hook): pin GIT_DIR", "area: what"},
		{"Merge pull request #13 from trick77/x", "area: what"},
		{"point hints at master", "area: what"},
		{"ship v0: check, hook", "area: what"},
		{"hook:pin", "area: what"},
		{"hook: ", "area: what"},
		{": nothing", "area: what"},
	} {
		err := Check(tc.s)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%q: %v", tc.s, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%q: got %v, want error with %q", tc.s, err, tc.want)
		}
	}
}

func TestRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []string
		bump     string
		notes    []string
		err      string
	}{
		{"none", []string{"hook: x\n\nbody\n", "gate: y\n"}, "patch", nil, ""},
		{"minor", []string{"hook: x\n\nRelease: minor\nRelease-note: hooks moved\n"}, "minor", []string{"hooks moved"}, ""},
		{"highest wins, notes in order", []string{
			"a: x\n\nRelease: major\nRelease-note: one\n",
			"b: y\n\nRelease: minor\nRelease-note: two\n",
		}, "major", []string{"one", "two"}, ""},
		{"note alone keeps patch", []string{"a: x\n\nRelease-note: fyi\n"}, "patch", []string{"fyi"}, ""},
		{"inside a line is no trailer", []string{"a: x\n\nsee Release: minor in docs\n"}, "patch", nil, ""},
		{"subject release: is no trailer", []string{"release: build first\n", "Release: minor\n"}, "patch", nil, ""},
		{"loose forms", []string{"a: x\r\n\r\n  Release:\tMinor (drops Old)\r\n\tRelease-note: Old is gone.  \r\n"},
			"minor", []string{"Old is gone."}, ""},
		{"squash body", []string{"a: x (#3)\n\n* a: x\n\n* b: y\n\nRelease: major\nRelease-note: n\n"}, "major", []string{"n"}, ""},
		{"minor without note", []string{"a: x\n\nRelease: minor\n"}, "minor", nil, "without a `Release-note"},
		{"typo, notes still read", []string{"a: x\n\nRelease: minr\nRelease-note: kept\n"}, "patch", []string{"kept"},
			"want `Release: minor`"},
		{"patch spelled out", []string{"a: x\n\nRelease: patch\n"}, "patch", nil, "patch is the default"},
		{"empty", []string{"a: x\n\nRelease:\n"}, "patch", nil, "want `Release: minor`"},
	} {
		bump, notes, err := Release(tc.messages)
		if bump != tc.bump || !slices.Equal(notes, tc.notes) {
			t.Errorf("%s: got %s %q, want %s %q", tc.name, bump, notes, tc.bump, tc.notes)
		}
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: got %v, want error with %q", tc.name, err, tc.err)
		}
	}
}

func TestParse(t *testing.T) {
	areas, text, ok := Parse("net/http, net/url: fix parsing (#7)")
	if !ok || !slices.Equal(areas, []string{"net/http", "net/url"}) || text != "fix parsing (#7)" {
		t.Errorf("got %v %q %v", areas, text, ok)
	}
	if _, _, ok := Parse("no area here"); ok {
		t.Error("parsed a subject without area")
	}
}
