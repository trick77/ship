package check

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/trick77/ship/internal/gitx"
)

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
