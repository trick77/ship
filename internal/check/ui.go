package check

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/pins"
)

// prettier checks that the UI pins prettier at ship's version exactly: a caret range lets two clones format the
// same file two ways, and every repo formats alike.
func (r runner) prettier() error {
	if r.c.UI == nil {
		fmt.Println("(no ui)")
		return nil
	}
	p := filepath.Join(r.root, r.c.UI.Dir, "package.json")
	b, err := os.ReadFile(p) //nolint:gosec // the repo's own package.json
	if err != nil {
		return err
	}
	var pkg struct {
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	v, ok := pkg.DevDependencies["prettier"]
	if !ok {
		v = "(none)"
	}
	if v != pins.Prettier {
		return fmt.Errorf("prettier %s, ship pins %s: devDependencies.prettier in %s/package.json must be exactly %s",
			v, pins.Prettier, r.c.UI.Dir, pins.Prettier)
	}
	fmt.Println("ship: prettier", v)
	return nil
}

// cleanEmbed empties ui.embed but for .gitkeep (which keeps the dir, and go:embed's pattern, in a fresh clone):
// a file an older build left there would otherwise ship in the binary. Refused: a dir outside the repo (symlinks
// resolved), the root, one holding the UI or Go sources, or tracked files other than .gitkeep.
func (r runner) cleanEmbed() error {
	if r.c.UI == nil || r.c.UI.Embed == "" {
		fmt.Println("(none)")
		return nil
	}
	e := r.c.UI.Embed
	dir := filepath.Join(r.root, e)
	rel, ok := gitx.Inside(r.root, dir)
	if !ok || rel == "." {
		return fmt.Errorf("ui.embed %s: not a directory inside the repo", e)
	}
	for _, src := range []string{r.c.UI.Dir, r.c.Go.Dir} {
		if _, in := gitx.Inside(dir, filepath.Join(r.root, src)); in {
			return fmt.Errorf("ui.embed %s holds sources (%s): it would be emptied", e, src)
		}
	}
	tracked, err := gitx.GitZ(r.root, "ls-files", "-z", "--", rel)
	if err != nil {
		return err
	}
	tracked = slices.DeleteFunc(tracked, func(f string) bool { return filepath.Base(f) == ".gitkeep" })
	if len(tracked) > 0 {
		return fmt.Errorf("ui.embed %s holds tracked files, only .gitkeep may be: %v", e, tracked)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, en := range entries {
		if en.Name() == ".gitkeep" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, en.Name())); err != nil {
			return err
		}
	}
	fmt.Printf("ship: emptied %s\n", e)
	return nil
}
