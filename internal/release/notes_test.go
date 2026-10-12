package release

import (
	"fmt"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

func TestNotes(t *testing.T) {
	got := Notes([]gitx.Commit{
		{Subject: "release: build first (#13)"},
		{Subject: "build(deps): bump x"},
		{Subject: "hook: pin GIT_DIR"},
		{Subject: "net/http, net/url: fix parsing"},
		{Subject: "hook: warn on a foreign hook"},
	})
	want := "**hook**\n- pin GIT_DIR\n- warn on a foreign hook\n\n" +
		"**net/http**\n- net/http, net/url: fix parsing\n\n" +
		"**release**\n- build first (#13)\n\n" +
		"**other**\n- build(deps): bump x"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if Notes(nil) != "" {
		t.Error("notes without commits")
	}
	var many []gitx.Commit
	for i := range MaxNotes + 3 {
		many = append(many, gitx.Commit{Subject: fmt.Sprintf("a: %d", i)})
	}
	if n := Notes(many); !strings.HasSuffix(n, "… and 3 more") || strings.Contains(n, fmt.Sprintf("- %d\n", MaxNotes)) {
		t.Errorf("cap:\n%s", n)
	}
}

func TestRun_whenTaggedBefore_thenNotesSinceThatTag(t *testing.T) {
	work := clone(t)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(work, args...); err != nil {
			t.Fatal(err)
		}
	}
	// An annotated tag: ls-remote lists the tag object, its ^{} line the commit the notes start from.
	git("tag", "-a", "-m", "v1", "v1.0.0")
	git("commit", "-q", "--allow-empty", "-m", "hook: pin GIT_DIR")
	git("commit", "-q", "--allow-empty", "-m", "no area")
	git("push", "-q", "--no-verify", "origin", "master", "v1.0.0")
	c, _ := config.Parse([]byte("tag_prefix: v\n"))
	out, err := capture(t, func() error { return Run(work, c, Options{Bump: "minor", DryRun: true}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--notes **hook**\n- pin GIT_DIR\n\n**other**\n- no area\n") || strings.Contains(out, "- two") {
		t.Fatalf("want notes since v1.0.0 only:\n%s", out)
	}
}
