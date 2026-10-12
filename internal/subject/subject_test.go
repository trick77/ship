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

func TestParse(t *testing.T) {
	areas, text, ok := Parse("net/http, net/url: fix parsing (#7)")
	if !ok || !slices.Equal(areas, []string{"net/http", "net/url"}) || text != "fix parsing (#7)" {
		t.Errorf("got %v %q %v", areas, text, ok)
	}
	if _, _, ok := Parse("no area here"); ok {
		t.Error("parsed a subject without area")
	}
}
