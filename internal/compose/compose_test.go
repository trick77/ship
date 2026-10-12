package compose

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/gitx"
)

func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gitx.Git(dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for p, s := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFiles_whenRootComposeNames_thenSorted(t *testing.T) {
	got := Files(map[string]bool{"docker-compose.override.yml": true, "compose.yaml": true, "deploy/compose.yaml": true,
		"stack.yaml": true, "compose.dev.yaml": true})
	if strings.Join(got, ",") != "compose.dev.yaml,compose.yaml,docker-compose.override.yml" {
		t.Error(got)
	}
}

func TestImage_whenServiceNamed_thenItsImage(t *testing.T) {
	dir := repo(t, map[string]string{
		"compose.yaml": "x-common: &c\n  restart: always\nservices:\n  app:\n    <<: *c\n    build: .\n" +
			"  db:\n    image: \"timescale/timescaledb:2.29.1-pg17\" # bumped by Dependabot\n",
		// Same image again: no conflict.
		"compose.override.yaml": "services:\n  db:\n    image: timescale/timescaledb:2.29.1-pg17\n",
		// An override without image: the other files name it.
		"compose.dev.yaml":    "services:\n  db:\n    ports: [\"5432:5432\"]\n",
		"deploy/compose.yaml": "services:\n  db:\n    image: postgres:16\n",
	})
	img, err := Image(dir, "db")
	if err != nil || img != "timescale/timescaledb:2.29.1-pg17" {
		t.Errorf("%q %v", img, err)
	}
}

func TestImage_whenUnresolvable_thenError(t *testing.T) {
	for _, tt := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"deploy/compose.yaml": "services:\n  db:\n    image: postgres:17\n"},
			"compose:db: no compose file at the repo root"},
		{map[string]string{"compose.yaml": "services:\n  app:\n    image: x:1\n  web:\n    image: y:1\n"},
			"compose:db: no such service in compose.yaml (services: app, web)"},
		{map[string]string{"compose.yaml": "services:\n  db:\n    build: .\n"}, "compose:db: service has no image: (compose.yaml)"},
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:${PG}\n"},
			`compose:db: image "postgres:${PG}" uses a variable`},
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: localhost:5000/postgres\n"},
			`compose:db: image "localhost:5000/postgres" has no tag or digest`},
		{map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:17\n",
			"compose.dev.yaml": "services:\n  db:\n    image: postgres:16\n"},
			`compose:db: image "postgres:16" (compose.dev.yaml) and "postgres:17" (compose.yaml) differ`},
		{map[string]string{"compose.yaml": "services: [\n"}, "compose:db: compose.yaml: yaml"},
	} {
		if _, err := Image(repo(t, tt.files), "db"); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: %v, want %q", tt.files, err, tt.want)
		}
	}
	if _, err := Image(t.TempDir(), "db"); err == nil {
		t.Error("no git repo: no error")
	}
}

func TestRepositoryAndPinned(t *testing.T) {
	for ref, want := range map[string]string{
		"postgres:17-alpine":                           "postgres",
		"docker.io/library/postgres@sha256:abc":        "postgres",
		"timescale/timescaledb:2.29.1-pg17@sha256:abc": "timescale/timescaledb",
		"localhost:5000/pg":                            "localhost:5000/pg",
	} {
		if got := Repository(ref); got != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
	for ref, want := range map[string]bool{"postgres:17": true, "pg@sha256:abc": true, "postgres": false,
		"localhost:5000/pg": false} {
		if Pinned(ref) != want {
			t.Errorf("%s: %v", ref, !want)
		}
	}
}
