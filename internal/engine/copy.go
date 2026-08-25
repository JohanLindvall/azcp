package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// transfer moves one file. It is called on a worker goroutine and may be called
// again for the same task if the first attempt failed transiently.
func (e *Engine) transfer(ctx context.Context, t *task, pt *progress.Task) error {
	if t.backup != "" {
		if err := os.Rename(t.dst.Path, t.backup); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cannot back up %s: %w", quote(t.dst.Display()), err)
		}
		// Only back up once, however many attempts this task takes.
		t.backup = ""
	}
	if t.removeFirst {
		if err := e.storeFor(t.dst).Remove(ctx, t.dst); err != nil &&
			!store.IsNotExist(err) && !os.IsNotExist(err) {
			return fmt.Errorf("cannot remove %s: %w", quote(t.dst.Display()), err)
		}
		t.removeFirst = false
	}

	srcRemote, dstRemote := t.src.URL.IsRemote(), t.dst.IsRemote()
	switch {
	case !srcRemote && !dstRemote:
		return e.copyLocal(ctx, t, pt)
	case !srcRemote && dstRemote:
		return e.upload(ctx, t, pt)
	case srcRemote && !dstRemote:
		return e.download(ctx, t, pt)
	default:
		return e.copyRemote(ctx, t, pt)
	}
}

func (e *Engine) transferOptions() azure.TransferOptions {
	return azure.TransferOptions{
		BlockSize:   e.opt.PartSize,
		Concurrency: e.opt.PartConcurrency,
		ContentType: e.opt.ContentType,
		AccessTier:  e.opt.AccessTier,
		NoClobber:   e.opt.NoClobber,
		PutMD5:      e.opt.PutMD5,
		CheckMD5:    e.opt.CheckMD5,
	}
}

// upload sends a local file to blob storage.
func (e *Engine) upload(ctx context.Context, t *task, pt *progress.Task) error {
	if err := e.az.MkdirAll(ctx, t.dst, 0); err != nil {
		return err
	}
	if e.opt.AttributesOnly {
		// There is nothing to set on a blob that does not exist, so this
		// degenerates to creating an empty one.
		opts := e.transferOptions()
		opts.Progress = pt.Set
		return e.az.UploadStream(ctx, strings.NewReader(""), t.dst, opts)
	}
	opts := e.transferOptions()
	opts.Progress = pt.Set
	return e.az.Upload(ctx, t.src.URL.Path, t.dst, opts)
}

// download fetches a blob into a local file.
func (e *Engine) download(ctx context.Context, t *task, pt *progress.Task) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if e.opt.NoClobber {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := e.openDest(t, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if !e.opt.AttributesOnly {
		opts := e.transferOptions()
		opts.Progress = pt.Set
		if err := e.az.Download(ctx, t.src, f, opts); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", quote(t.dst.Display()), err)
	}
	if e.opt.Preserve.Timestamps && !t.src.ModTime.IsZero() {
		if err := os.Chtimes(t.dst.Path, t.src.ModTime, t.src.ModTime); err != nil {
			e.log.Warn("cannot preserve timestamp",
				"path", t.dst.Display(), "error", err)
		}
	}
	return nil
}

// copyRemote copies blob to blob, preferring to have the service move the bytes.
func (e *Engine) copyRemote(ctx context.Context, t *task, pt *progress.Task) error {
	if err := e.az.MkdirAll(ctx, t.dst, 0); err != nil {
		return err
	}
	opts := e.transferOptions()
	opts.Progress = pt.Set
	return e.az.Copy(ctx, t.src, t.dst, opts)
}

// copyLocal handles the filesystem-to-filesystem case, including the link and
// attribute-only variants cp offers.
func (e *Engine) copyLocal(ctx context.Context, t *task, pt *progress.Task) error {
	srcPath, dstPath := t.src.URL.Path, t.dst.Path

	switch {
	case e.opt.SymbolicLink:
		return e.replace(t, func() error {
			target := srcPath
			if !filepath.IsAbs(target) {
				abs, err := filepath.Abs(target)
				if err != nil {
					return err
				}
				target = abs
			}
			return os.Symlink(target, dstPath)
		})
	case e.opt.HardLink && !t.src.IsSymlink():
		return e.replace(t, func() error { return os.Link(srcPath, dstPath) })
	case t.src.IsSymlink():
		return e.replace(t, func() error { return local.CopySymlink(srcPath, dstPath) })
	}

	// Files that were hard-linked in the source stay linked in the copy.
	if e.opt.Preserve.Links {
		if done, err := e.relinkExisting(t); done || err != nil {
			return err
		}
	}

	if e.opt.AttributesOnly {
		f, err := e.openDest(t, os.O_WRONLY|os.O_CREATE, t.src.Mode.Perm())
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	} else {
		opts := local.CopyOptions{
			Reflink:  e.opt.Reflink,
			Sparse:   e.opt.Sparse,
			Mode:     t.src.Mode.Perm(),
			Progress: pt.Add,
			Excl:     e.opt.NoClobber,
		}
		_, err := local.CopyFile(ctx, srcPath, dstPath, opts)
		if err != nil {
			if !e.forceRetryable(err) {
				return destError(t, err)
			}
			// -f: the destination exists but cannot be opened for writing.
			// Remove it and try once more, which is what cp does.
			e.log.Info("removing unwritable destination and retrying",
				"path", dstPath, "error", err)
			if rmErr := os.Remove(dstPath); rmErr != nil {
				return err
			}
			if _, err = local.CopyFile(ctx, srcPath, dstPath, opts); err != nil {
				return destError(t, err)
			}
		}
	}

	e.applyAttrs(t)
	e.rememberLink(t)
	return nil
}

// replace performs an operation that cannot overwrite in place, removing an
// existing destination first the way cp does for links.
func (e *Engine) replace(t *task, fn func() error) error {
	err := fn()
	if err == nil {
		e.applyAttrs(t)
		return nil
	}
	if !errors.Is(err, os.ErrExist) || e.opt.NoClobber {
		return err
	}
	if rmErr := os.Remove(t.dst.Path); rmErr != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	e.applyAttrs(t)
	return nil
}

// openDest opens a local destination, applying -f by clearing an unwritable
// file out of the way.
func (e *Engine) openDest(t *task, flags int, mode os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(t.dst.Path, flags, mode)
	if err == nil {
		return f, nil
	}
	if e.forceRetryable(err) {
		if rmErr := os.Remove(t.dst.Path); rmErr == nil {
			if f, err2 := os.OpenFile(t.dst.Path, flags, mode); err2 == nil {
				e.log.Info("removed unwritable destination", "path", t.dst.Path)
				return f, nil
			}
		}
	}
	return nil, destError(t, err)
}

// destError phrases a destination failure the way cp does.
func destError(t *task, err error) error {
	if errors.Is(err, syscall.EISDIR) {
		return plainf("cannot overwrite directory %s with non-directory",
			quote(t.dst.Display()))
	}
	return fmt.Errorf("cannot create %s: %w", quote(t.dst.Display()), err)
}

// forceRetryable reports whether -f should clear the destination and try again.
func (e *Engine) forceRetryable(err error) bool {
	if !e.opt.Force {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.EACCES, syscall.EPERM, syscall.ETXTBSY:
		return true
	}
	return false
}

// relinkExisting recreates a hard link when this source inode has already been
// copied. It returns true when the destination is now in place.
func (e *Engine) relinkExisting(t *task) (bool, error) {
	info, err := os.Lstat(t.src.URL.Path)
	if err != nil {
		return false, nil
	}
	id, nlink, ok := local.IDOf(info)
	if !ok || nlink < 2 {
		return false, nil
	}
	e.hardLinksMu.Lock()
	first, seen := e.hardLinks[id]
	e.hardLinksMu.Unlock()
	if !seen {
		return false, nil
	}
	if err := e.replace(t, func() error { return os.Link(first, t.dst.Path) }); err != nil {
		e.log.Warn("cannot preserve hard link, copying instead",
			"source", t.src.URL.Display(), "first_copy", first, "error", err)
		return false, nil
	}
	return true, nil
}

func (e *Engine) rememberLink(t *task) {
	if !e.opt.Preserve.Links {
		return
	}
	info, err := os.Lstat(t.src.URL.Path)
	if err != nil {
		return
	}
	id, nlink, ok := local.IDOf(info)
	if !ok || nlink < 2 {
		return
	}
	e.hardLinksMu.Lock()
	if _, exists := e.hardLinks[id]; !exists {
		e.hardLinks[id] = t.dst.Path
	}
	e.hardLinksMu.Unlock()
}

// applyAttrs copies the requested attributes onto a local destination.
func (e *Engine) applyAttrs(t *task) {
	if !e.opt.Preserve.Any() || t.dst.IsRemote() || t.src.URL.IsRemote() {
		return
	}
	info, err := os.Lstat(t.src.URL.Path)
	if err != nil {
		e.log.Warn("cannot read source attributes",
			"path", t.src.URL.Display(), "error", err)
		return
	}
	errs := local.ApplyAttrs(t.src.URL.Path, t.dst.Path, info, e.opt.Preserve, t.src.IsSymlink())
	for _, err := range errs {
		e.log.Warn("cannot preserve attribute",
			"path", t.dst.Display(), "error", err)
	}
}

// checkUnsupported rejects combinations that cannot work against blob storage
// before any data moves, rather than failing partway through.
func (e *Engine) checkUnsupported(dest *uri.URL) error {
	if e.opt.SELinux {
		// Accepted so existing command lines keep working, but nothing here
		// sets a security context, and silently doing less than asked would be
		// worse than saying so.
		e.log.Warn("ignoring -Z and --context: this tool does not set SELinux contexts")
	}
	if e.opt.Preserve.Context {
		e.log.Warn("ignoring --preserve=context: this tool does not copy SELinux contexts")
	}
	remote := dest.IsRemote()
	srcRemote := false
	for _, s := range e.opt.Sources {
		if uri.IsRemoteArg(s) {
			remote, srcRemote = true, true
		}
	}
	if e.opt.BandwidthLimit > 0 && srcRemote && dest.IsRemote() {
		// The service moves these bytes between its own machines; they never
		// reach this process, so there is nothing here to pace.
		e.log.Warn("--bwlimit does not apply to a server-side blob-to-blob copy: " +
			"the data never passes through this host")
	}
	if !remote {
		return nil
	}
	switch {
	case e.opt.HardLink:
		return fmt.Errorf("--link is not possible with blob storage")
	case e.opt.SymbolicLink:
		return fmt.Errorf("--symbolic-link is not possible with blob storage")
	case e.opt.Backup != cli.BackupNone && dest.IsRemote():
		return fmt.Errorf("--backup is not supported for blob destinations")
	}
	return nil
}

// backupName picks the name an existing destination is moved to.
func (e *Engine) backupName(path string) (string, error) {
	switch e.opt.Backup {
	case cli.BackupSimple:
		return path + e.opt.Suffix, nil
	case cli.BackupNumbered:
		return nextNumbered(path)
	case cli.BackupExisting:
		if hasNumberedBackups(path) {
			return nextNumbered(path)
		}
		return path + e.opt.Suffix, nil
	}
	return "", fmt.Errorf("no backup requested")
}

func hasNumberedBackups(path string) bool {
	matches, err := filepath.Glob(path + ".~*~")
	return err == nil && len(matches) > 0
}

func nextNumbered(path string) (string, error) {
	highest := 0
	matches, err := filepath.Glob(path + ".~*~")
	if err != nil {
		return "", err
	}
	for _, m := range matches {
		num := strings.TrimSuffix(strings.TrimPrefix(m, path+".~"), "~")
		if n, err := strconv.Atoi(num); err == nil && n > highest {
			highest = n
		}
	}
	return fmt.Sprintf("%s.~%d~", path, highest+1), nil
}
