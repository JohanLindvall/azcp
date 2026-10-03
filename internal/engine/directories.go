package engine

import (
	"context"
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// --parents carries each intermediate source directory's permissions and
// requested attributes too. Making all parents with a fixed mode loses that
// information and leaves restrictive directories impossible to populate.
func (e *Engine) prepareParents(ctx context.Context, src *store.Node, dest *uri.URL, parts []string) error {
	if src.URL.IsRemote() {
		parent := dest.Join(parts...)
		return e.storeFor(parent).MkdirAll(ctx, parent, 0o755)
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
		if err := e.prepareDirectory(ctx, parent, dest.Join(parts[:i+1]...)); err != nil {
			return err
		}
	}
	return nil
}

// prepareDirectory leaves existing permissions alone unless --preserve asks
// otherwise. A new local directory starts with the source's mode and the
// process umask, just like a newly copied regular file.
func (e *Engine) prepareDirectory(ctx context.Context, src *store.Node, dst *uri.URL) error {
	if dst.IsRemote() {
		return e.az.MkdirAll(ctx, dst, 0)
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
