// Package fsx holds file helpers the standard library leaves out.
package fsx

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data so a reader, or the next start after a crash, sees the old file or the new
// one, never a torn one. The temp file is created beside path (a rename across filesystems is a copy, not atomic),
// fsynced before the rename (or a crash can leave the new name on empty blocks), and the directory is fsynced after
// it, best-effort. A failure leaves path untouched and no temp file behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename is durable only once the directory entry is; some platforms cannot fsync a directory, and the
	// rename already happened, so a failure here is not the caller's.
	if d, derr := os.Open(dir); derr == nil { //nolint:gosec // the caller's own target directory
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
