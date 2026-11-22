package check

import (
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

func TestIsDoc(t *testing.T) {
	withUI, err := config.Parse([]byte("ui: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	rootUI, err := config.Parse([]byte("ui:\n  dir: .\n"))
	if err != nil {
		t.Fatal(err)
	}
	goOnly, err := config.Parse([]byte("go: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	embed, err := config.Parse([]byte("ui:\n  embed: backend/web/dist\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		c    *config.Config
		p    string
		want bool
	}{
		{withUI, "README.md", true},
		{withUI, "docs/guide.MD", true},
		{withUI, "LICENSE", true},
		{withUI, "docs/shot.PNG", true},
		{withUI, "uikit/logo.svg", true},
		{withUI, "ui/README.md", false},
		{withUI, "ui/src/logo.svg", false},
		{withUI, "ui/public/favicon.png", false},
		{rootUI, "docs/shot.png", false},
		{embed, "backend/web/dist/logo.png", false},
		{embed, "backend/README.md", true},
		{goOnly, "ui/logo.png", true},
		{withUI, "kit/schemadoc/testdata/fixture.md", false},
		{withUI, "testdata/x.png", false},
		{withUI, "main.go", false},
		{withUI, "ship.yaml", false},
		{withUI, ".github/workflows/check.yaml", false},
		{withUI, "LICENSE.txt", false},
		{withUI, "Makefile", false},
	} {
		if got := (runner{c: tc.c}).isDoc(tc.p); got != tc.want {
			t.Errorf("isDoc(%q, ui %v) = %v, want %v", tc.p, tc.c.UI != nil, got, tc.want)
		}
	}
}

func TestDocsOnly(t *testing.T) {
	c, err := config.Parse([]byte("ui: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		branch func(t *testing.T, dir string)
		want   bool
	}{
		{"nothing changed", func(*testing.T, string) {}, false},
		{"readme committed", func(t *testing.T, dir string) {
			writeFile(t, dir, "README.md", "# m\n\nmore\n")
			docGit(t, dir, "commit", "-qam", "docs: more")
		}, true},
		{"readme unstaged, image untracked", func(t *testing.T, dir string) {
			writeFile(t, dir, "README.md", "# m\n\nmore\n")
			writeFile(t, dir, "docs/shot.png", "png")
		}, true},
		{"go file untracked", func(t *testing.T, dir string) {
			writeFile(t, dir, "README.md", "# m\n\nmore\n")
			writeFile(t, dir, "lib/b.go", "package lib\n")
		}, false},
		{"go file deleted", func(t *testing.T, dir string) {
			docGit(t, dir, "rm", "-q", "lib/a.go")
			docGit(t, dir, "commit", "-qm", "lib: drop a")
		}, false},
		{"go file renamed to md", func(t *testing.T, dir string) {
			docGit(t, dir, "mv", "lib/a.go", "lib/a.md")
			docGit(t, dir, "commit", "-qm", "lib: a is docs")
		}, false},
		{"ui image", func(t *testing.T, dir string) {
			writeFile(t, dir, "ui/src/logo.svg", "<svg/>")
		}, false},
		{"go change committed, undone in the tree", func(t *testing.T, dir string) {
			writeFile(t, dir, "lib/a.go", "package lib\n\nconst A = 1\n")
			writeFile(t, dir, "README.md", "# m\n\nmore\n")
			docGit(t, dir, "commit", "-qam", "lib: A")
			docGit(t, dir, "checkout", "origin/master", "--", "lib/a.go")
		}, false},
		{"readme a test reads", func(t *testing.T, dir string) {
			writeFile(t, dir, "lib/a_test.go", "package lib\n\nconst readme = \"../README.md\"\n")
			docGit(t, dir, "add", "-A")
			docGit(t, dir, "commit", "-qm", "lib: read README")
			docGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
			writeFile(t, dir, "README.md", "# m\n\nmore\n")
		}, false},
		{"readme another doc links", func(t *testing.T, dir string) {
			writeFile(t, dir, "docs/a.md", "see [README.md](../README.md)\n")
			writeFile(t, dir, "README.md", "# m\n\nmore\n")
		}, true},
		{"image a go:embed dir takes", func(t *testing.T, dir string) {
			writeFile(t, dir, "lib/embed.go", "package lib\n\nimport \"embed\"\n\n//go:embed \"all:static\"\nvar static embed.FS\n")
			docGit(t, dir, "add", "-A")
			docGit(t, dir, "commit", "-qm", "lib: static")
			docGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
			writeFile(t, dir, "lib/static/img/logo.png", "png")
		}, false},
		{"image beside a go:embed it is not in", func(t *testing.T, dir string) {
			writeFile(t, dir, "lib/embed.go", "package lib\n\nimport \"embed\"\n\n//go:embed static/*.html\nvar static embed.FS\n")
			docGit(t, dir, "add", "-A")
			docGit(t, dir, "commit", "-qm", "lib: static")
			docGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
			writeFile(t, dir, "lib/static/logo.png", "png")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			docGit(t, dir, "init", "-q", "-b", "master")
			docGit(t, dir, "config", "user.email", "t@example.com")
			docGit(t, dir, "config", "user.name", "t")
			docGit(t, dir, "config", "commit.gpgsign", "false")
			writeFile(t, dir, "README.md", "# m\n")
			writeFile(t, dir, "lib/a.go", "package lib\n")
			docGit(t, dir, "add", "-A")
			docGit(t, dir, "commit", "-qm", "base")
			docGit(t, dir, "update-ref", "refs/remotes/origin/master", "HEAD")
			tc.branch(t, dir)
			got, err := (runner{root: dir, c: c}).docsOnly()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("docsOnly = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDocsOnly_whenNoBase_thenWholeTreeCounts(t *testing.T) {
	dir := t.TempDir()
	docGit(t, dir, "init", "-q", "-b", "master")
	docGit(t, dir, "config", "user.email", "t@example.com")
	docGit(t, dir, "config", "user.name", "t")
	docGit(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, dir, "README.md", "# m\n")
	docGit(t, dir, "add", "-A")
	docGit(t, dir, "commit", "-qm", "base")
	got, err := (runner{root: dir, c: &config.Config{}, noBase: true}).docsOnly()
	if err != nil || !got {
		t.Fatalf("docsOnly = %v, %v; want true", got, err)
	}
	if _, err := (runner{root: dir, c: &config.Config{Base: "master"}}).docsOnly(); err == nil {
		t.Fatal("no origin/master: want the merge-base error")
	}
}

func docGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := gitx.Git(dir, args...); err != nil {
		t.Fatal(err)
	}
}
