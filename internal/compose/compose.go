// Package compose reads the images of a repo's root compose files: the files Dependabot's docker-compose ecosystem
// bumps at "/", so an image named there is the one kept current.
package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/trick77/ship/internal/gitx"
)

// file: the names Dependabot's docker-compose ecosystem reads at a directory (compose.yaml, compose.dev.yaml,
// docker-compose.override.yml).
var file = regexp.MustCompile(`^(?:docker-)?compose(?:[.-][\w.-]+)?\.ya?ml$`)

// Files returns the root compose files of listed (git's view of the tree: tracked and untracked, not ignored),
// sorted.
func Files(listed map[string]bool) []string {
	var fs []string
	for f := range listed {
		if !strings.Contains(f, "/") && file.MatchString(f) {
			fs = append(fs, f)
		}
	}
	slices.Sort(fs)
	return fs
}

// Service is one compose service's image, as written ("" for a build-only service).
type Service struct {
	Name, Image, File string
}

// Services reads the services of files (repo-relative), in file order, then by name. A file gone from disk (tracked
// but deleted, git still lists it) is skipped.
func Services(root string, files []string) ([]Service, error) {
	var out []Service
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f)) //nolint:gosec // the repo's own compose files
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var doc struct {
			Services map[string]struct {
				Image string `yaml:"image"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		names := make([]string, 0, len(doc.Services))
		for n := range doc.Services {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			out = append(out, Service{Name: n, Image: doc.Services[n].Image, File: f})
		}
	}
	return out, nil
}

// Image returns service's image from root's compose files. Missing, build-only, untagged, ${VAR} or named
// differently in two files: an error, never a guess.
func Image(root, service string) (string, error) {
	listed, err := gitx.Files(root)
	if err != nil {
		return "", err
	}
	var files []string
	for _, f := range Files(listed) {
		if _, err := os.Stat(filepath.Join(root, f)); err == nil {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("compose:%s: no compose file at the repo root", service)
	}
	all, err := Services(root, files)
	if err != nil {
		return "", fmt.Errorf("compose:%s: %w", service, err)
	}
	var img, in string
	var names, defined []string
	for _, s := range all {
		names = append(names, s.Name)
		if s.Name != service {
			continue
		}
		defined = append(defined, s.File)
		switch {
		case s.Image == "":
			continue // an override file's ports or env: the image is another file's
		case strings.Contains(s.Image, "$"):
			return "", fmt.Errorf("compose:%s: image %q uses a variable, ship cannot resolve it (%s)", service, s.Image, s.File)
		case !Pinned(s.Image):
			return "", fmt.Errorf("compose:%s: image %q has no tag or digest (%s)", service, s.Image, s.File)
		case img != "" && img != s.Image:
			return "", fmt.Errorf("compose:%s: image %q (%s) and %q (%s) differ", service, img, in, s.Image, s.File)
		}
		img, in = s.Image, s.File
	}
	if img == "" && len(defined) > 0 {
		return "", fmt.Errorf("compose:%s: service has no image: (%s)", service, strings.Join(defined, ", "))
	}
	if img == "" {
		slices.Sort(names)
		return "", fmt.Errorf("compose:%s: no such service in %s (services: %s)", service,
			strings.Join(files, ", "), strings.Join(slices.Compact(names), ", "))
	}
	return img, nil
}

// Pinned reports whether ref names a tag or a digest: an untagged ref is :latest, a moving target.
func Pinned(ref string) bool {
	return strings.Contains(ref, "@") || strings.LastIndexByte(ref, ':') > strings.LastIndexByte(ref, '/')
}

// Repository is ref without tag and digest, Docker Hub's docker.io/ and library/ dropped: postgres:17-alpine,
// docker.io/library/postgres@sha256:… and postgres are one repository.
func Repository(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref = ref[:i]
	}
	return strings.TrimPrefix(strings.TrimPrefix(ref, "docker.io/"), "library/")
}
