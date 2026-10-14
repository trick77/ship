package release

import (
	"fmt"
	"path"
	"path/filepath"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// nested is c narrowed to the Go module at dir (-dir): tags <dir>/<tag_prefix>X.Y.Z, go build in dir, no images and
// no UI (those are the repo's). dir is repo-relative, cleaned; empty or "." is the repo itself, c unchanged.
func nested(c *config.Config, dir string) (*config.Config, string, error) {
	d := path.Clean(filepath.ToSlash(dir))
	if dir == "" || d == "." {
		return c, "", nil
	}
	if config.Escapes(d) {
		return nil, "", fmt.Errorf("-dir %s: want a dir inside the repo", dir)
	}
	// Go resolves a nested module's version only from <dir>/vX.Y.Z: any other prefix tags a version go get never sees.
	if c.TagPrefix != "v" {
		return nil, "", fmt.Errorf("-dir %s: a nested Go module's tags are %s/vX.Y.Z, set tag_prefix: v", dir, d)
	}
	n := *c
	n.TagPrefix, n.Images, n.UI, n.Go.Dir = d+"/"+c.TagPrefix, nil, nil, d
	return &n, d, nil
}

// scope is the pathspecs a release covers, nil for the whole repo: its module dir minus the modules nested below
// it, each released on its own (kit's release ignores kit/auth/, the root's kit/). The root's own scope stays the
// whole repo (ui/, Containerfile, workflows ship too), only the nested modules' dirs cut out; with images nothing
// is cut out, a build context may copy any dir.
func scope(root string, c *config.Config, dir string) ([]string, error) {
	if dir == "" && len(c.Images) > 0 {
		return nil, nil
	}
	mod := c.Go.Dir
	// No module at go.dir (a repo of no Go, or go.dir unset with backend/go.mod): nothing is nested in it.
	if !fileExists(filepath.Join(root, mod, "go.mod")) {
		return nil, nil
	}
	pattern := "*/go.mod"
	if mod != "." {
		pattern = mod + "/" + pattern
	}
	// A pathspec's * crosses /: kit/*/go.mod finds kit/auth/go.mod and deeper ones.
	mods, err := gitx.GitZ(root, "ls-files", "-z", "--", pattern)
	if err != nil {
		return nil, err
	}
	paths := []string{"."}
	if dir != "" {
		paths = []string{dir}
	}
	for _, m := range mods {
		paths = append(paths, ":(exclude)"+path.Dir(m))
	}
	if dir == "" && len(paths) == 1 {
		return nil, nil
	}
	return paths, nil
}
