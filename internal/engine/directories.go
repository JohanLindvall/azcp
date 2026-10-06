// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// --parents carries each intermediate source directory's permissions and
// requested attributes too. Making all parents with a fixed mode loses that
// information and leaves restrictive directories impossible to populate.
func (e *Engine) prepareParents(ctx context.Context, src *store.Node, dest *uri.URL, parts []string) error {
	parent := dest.Join(parts...)
	if src.URL.IsRemote() {
		return e.storeFor(parent).MkdirAll(ctx, parent, 0o755)
	}
	if !parent.IsRemote() {
		if info, err := os.Stat(parent.Path); err == nil && info.IsDir() {
			// GNU cp leaves the ancestors' attributes alone when the complete
			// parent path already exists. Only creating parents preserves them.
			return nil
		}
	}
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		if strings.HasPrefix(src.URL.Path, "/") {
			prefix = "/" + prefix
		}
		parent, err := e.local.Stat(ctx, src.URL.WithPathPart(prefix), true)
		if err != nil {
			return err
		}
		if err := e.prepareDirectory(ctx, parent, dest.Join(parts[:i+1]...), true); err != nil {
			return err
		}
	}
	return nil
}

// prepareDirectory leaves existing permissions alone unless --preserve asks
// otherwise. A new local directory starts with the source's mode and the
// process umask, just like a newly copied regular file.
func (e *Engine) prepareDirectory(ctx context.Context, src *store.Node, dst *uri.URL, parent bool) error {
	if dst.IsRemote() {
		if e.opt.DryRun {
			return nil
		}
		return e.az.MkdirAll(ctx, dst, 0)
	}
	checkExisting := func() error {
		stat := os.Lstat
		if parent {
			// --parents reproduces the path leading to an operand; cp permits
			// existing directory links along that path, as in the target root.
			stat = os.Stat
		}
		info, err := stat(dst.Path)
		if err != nil {
			return err
		}
		// A directory being copied must not write through a destination
		// symlink. The containing directory explicitly named as an operand
		// has already been resolved by targetFor, as GNU cp permits.
		if !info.IsDir() {
			return plainf("cannot overwrite non-directory %s with directory %s",
				quote(dst.Display()), quote(src.URL.Display()))
		}
		return nil
	}
	if e.opt.DryRun {
		err := checkExisting()
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	mode := os.FileMode(0o755)
	if !src.URL.IsRemote() {
		mode = e.opt.CreationMode(src.Mode)
	}
	err := os.Mkdir(dst.Path, mode)
	created := err == nil
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
		if err := checkExisting(); err != nil {
			return err
		}
	}
	if src.URL.IsRemote() {
		return nil
	}
	info, err := sourceAttrs(src)
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

func (e *Engine) directoryFailure(dst *uri.URL, err error) {
	var plain *plainError
	if errors.As(err, &plain) {
		e.fail("%v", err)
	} else {
		e.fail("cannot create directory %s: %s", quote(dst.Display()), brief(err))
	}
}
