package engine

import (
	"context"
	"os"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// prepareDirectory leaves existing permissions alone unless --preserve asks
// otherwise. A new local directory starts with the source's mode and the
// process umask, just like a newly copied regular file.
func (e *Engine) prepareDirectory(ctx context.Context, src *store.Node, dst *uri.URL) error {
	if dst.IsRemote() {
		return e.az.MkdirAll(ctx, dst, 0)
	}
	mode := os.FileMode(0o755)
	if !src.URL.IsRemote() {
		mode = src.Mode.Perm()
	}
	err := os.Mkdir(dst.Path, mode)
	created := err == nil
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
		info, statErr := os.Stat(dst.Path)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return err
		}
	}
	if src.URL.IsRemote() {
		return nil
	}
	info, err := sourceInfo(src)
	if err != nil {
		return err
	}
	d := deferredDir{source: src.URL.Path, path: dst.Path, info: info}
	if created {
		info, err := os.Stat(dst.Path)
		if err != nil {
			return err
		}
		d.mode = info.Mode()
		d.restoreMode = d.mode.Perm()&0o700 != 0o700
		if d.restoreMode {
			if err := os.Chmod(dst.Path, d.mode|0o700); err != nil {
				return err
			}
		}
	}
	// Register before reading the children so errors and cancellation still
	// restore temporary permissions and preserved directory attributes.
	if d.restoreMode || e.opt.Preserve.Any() {
		e.deferredDirs = append(e.deferredDirs, d)
	}
	return nil
}
