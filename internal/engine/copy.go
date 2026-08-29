package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

	// A blob that records a symbolic link has no content to fetch; what it
	// says is where the link should point.
	if t.src.URL.IsRemote() && !t.dst.IsRemote() {
		if p := store.DecodePosixMeta(t.src.Metadata); p.IsSymlink() {
			return e.replace(t, func() error {
				return os.Symlink(p.SymlinkDest, t.dst.Path)
			})
		}
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
		Resume:      e.opt.Resume,
		PutMD5:      e.opt.PutMD5,
		CheckMD5:    e.opt.CheckMD5,

		ContentEncoding:    e.opt.ContentEncoding,
		ContentDisposition: e.opt.ContentDisposition,
		ContentLanguage:    e.opt.ContentLanguage,
		CacheControl:       e.opt.CacheControl,
	}
}

// upload sends a local file to blob storage.
func (e *Engine) upload(ctx context.Context, t *task, pt *progress.Task) error {
	if err := e.az.MkdirAll(ctx, t.dst, 0); err != nil {
		return err
	}
	if e.opt.AttributesOnly {
		// The attributes, not the data: an existing blob keeps its content and
		// has its metadata and headers replaced. Only when nothing is there
		// does this degenerate to creating an empty blob, as cp creates an
		// empty file.
		opts := e.transferOptions()
		opts.Metadata = e.uploadMetadata(t.src)
		return e.az.PutAttrs(ctx, t.dst, opts)
	}
	opts := e.transferOptions()
	opts.Progress = pt.Set
	opts.Metadata = e.uploadMetadata(t.src)

	// A symbolic link has no content beyond where it points, so it is stored
	// as an empty blob whose metadata says what it is.
	if t.src.IsSymlink() {
		return e.az.PutMarker(ctx, t.dst, opts)
	}
	return e.az.Upload(ctx, t.src.URL.Path, t.dst, opts)
}

// download fetches a blob into a local file.
func (e *Engine) download(ctx context.Context, t *task, pt *progress.Task) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	switch {
	case e.opt.AttributesOnly:
		// The contents stay as they are; only the attributes are wanted.
		flags = os.O_WRONLY | os.O_CREATE
	case e.opt.Resume:
		// Truncating would destroy the very bytes the resume record vouches
		// for. The ranges still to come are written where they belong.
		//
		// This wins over -n, which would refuse to open a file that is already
		// there: the only tasks that reach here with -n set are ones the
		// scanner let through, and the sole reason it lets an existing
		// destination through is that a record says the download never
		// finished. Refusing it would leave it unfinished for good.
		flags = os.O_WRONLY | os.O_CREATE
	case e.opt.NoClobber:
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
	if e.opt.Decompress && decompressible(t.src.ContentEncoding) {
		final, derr := decompressFile(t.dst.Path, t.src.ContentEncoding)
		if derr != nil {
			return derr
		}
		if final != t.dst.Path {
			e.log.Debug("expanded on arrival",
				"blob", t.src.URL.Display(), "path", final)
			t.dst = t.dst.WithPathPart(final)
		}
	}
	// restoreAttrs covers the timestamps too, including falling back to the
	// service's own when the blob carries none of its own.
	e.restoreAttrs(t)
	return nil
}

// copyRemote copies blob to blob, preferring to have the service move the bytes.
func (e *Engine) copyRemote(ctx context.Context, t *task, pt *progress.Task) error {
	if err := e.az.MkdirAll(ctx, t.dst, 0); err != nil {
		return err
	}
	opts := e.transferOptions()
	opts.Progress = pt.Set
	// Only some of the copy routes carry properties and metadata across on
	// their own — the asynchronous Copy Blob does, staging blocks does not —
	// so the source's are carried explicitly and every route preserves them.
	// Anything the user set on the command line still wins.
	if opts.ContentType == "" {
		opts.ContentType = t.src.ContentType
	}
	if opts.ContentEncoding == "" {
		opts.ContentEncoding = t.src.ContentEncoding
	}
	if opts.ContentDisposition == "" {
		opts.ContentDisposition = t.src.ContentDisposition
	}
	if opts.ContentLanguage == "" {
		opts.ContentLanguage = t.src.ContentLanguage
	}
	if opts.CacheControl == "" {
		opts.CacheControl = t.src.CacheControl
	}
	// The source's metadata is on the node only when the scan fetched it
	// (-a, --preserve or --copy-metadata); without that, the routes where the
	// service copies metadata itself still preserve it, and the staged and
	// streamed routes keep only what the service carries.
	opts.Metadata = t.src.Metadata
	if len(e.opt.Metadata) > 0 {
		// --metadata merges over what the source carries, exactly as an
		// upload merges it over the preserved attributes.
		merged := make(map[string]string, len(t.src.Metadata)+len(e.opt.Metadata))
		maps.Copy(merged, t.src.Metadata)
		maps.Copy(merged, e.opt.Metadata)
		opts.Metadata = merged
	}
	return e.az.Copy(ctx, t.src, t.dst, opts)
}

// copyLocal handles the filesystem-to-filesystem case, including the link and
// attribute-only variants cp offers.
func (e *Engine) copyLocal(ctx context.Context, t *task, pt *progress.Task) (retErr error) {
	srcPath, dstPath := t.src.URL.Path, t.dst.Path

	switch {
	case e.opt.SymbolicLink:
		// cp refuses a relative source unless the link lands in the current
		// directory, because the link text would dangle. Making the target
		// absolute instead produces a working link in exactly the cases cp
		// errors on, and the identical link everywhere else.
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

	// Files that were hard-linked in the source stay linked in the copy. The
	// first task for an identity claims it and copies; the rest wait for that
	// copy and link to it.
	if e.opt.Preserve.Links {
		linked, c, err := e.awaitHardLink(ctx, t)
		if linked || err != nil {
			return err
		}
		if c != nil {
			defer func() { e.settleLink(c, t.dst.Path, retErr == nil) }()
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
	return nil
}

// linkFuture is the promise the first copy of a hard-linked file makes to the
// others: done closes once the copy has settled, and path names the file to
// link to when ok says it landed.
type linkFuture struct {
	done chan struct{}
	path string
	ok   bool
}

// linkClaim is the claimant's handle on its own promise.
type linkClaim struct {
	id  local.FileID
	fut *linkFuture
}

// awaitHardLink decides how this task takes part in hard-link preservation.
// It returns linked=true when the destination is already in place as a link,
// a claim when this task is the one that must copy the data, and neither for
// a file with a single name.
func (e *Engine) awaitHardLink(ctx context.Context, t *task) (linked bool, claim *linkClaim, err error) {
	info, statErr := os.Lstat(t.src.URL.Path)
	if statErr != nil {
		return false, nil, nil // let the copy itself report the problem
	}
	id, nlink, ok := local.IDOf(t.src.URL.Path, info)
	if !ok || nlink < 2 {
		return false, nil, nil
	}

	e.hardLinksMu.Lock()
	fut := e.hardLinks[id]
	if fut == nil {
		fut = &linkFuture{done: make(chan struct{})}
		e.hardLinks[id] = fut
		e.hardLinksMu.Unlock()
		return false, &linkClaim{id: id, fut: fut}, nil
	}
	e.hardLinksMu.Unlock()

	// Tasks are handed to workers in the order they were queued, so the
	// claimant is already running (or finished) by the time this one starts:
	// waiting on it cannot deadlock, and a cancelled run unblocks everybody.
	select {
	case <-fut.done:
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
	if !fut.ok {
		// The first copy failed; copying the data is the best that is left.
		return false, nil, nil
	}
	if err := e.replace(t, func() error { return os.Link(fut.path, t.dst.Path) }); err != nil {
		e.log.Warn("cannot preserve hard link, copying instead",
			"source", t.src.URL.Display(), "first_copy", fut.path, "error", err)
		return false, nil, nil
	}
	return true, nil, nil
}

// settleLink resolves a claim. A failed copy gives the identity back, so a
// retry of this task — or a later name of the same file — can claim it afresh;
// anyone already waiting copies the data themselves.
func (e *Engine) settleLink(c *linkClaim, path string, ok bool) {
	if ok {
		c.fut.path, c.fut.ok = path, true
	} else {
		e.hardLinksMu.Lock()
		if e.hardLinks[c.id] == c.fut {
			delete(e.hardLinks, c.id)
		}
		e.hardLinksMu.Unlock()
	}
	close(c.fut.done)
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
	if e.opt.Preserve.Context && e.opt.ContextExplicit {
		// Only when asked for by name: --preserve=all sweeps it in, and
		// warning on every -a would be noise about something never mentioned.
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
	n, err := highestNumberedBackup(path)
	return err == nil && n > 0
}

func nextNumbered(path string) (string, error) {
	n, err := highestNumberedBackup(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.~%d~", path, n+1), nil
}

// highestNumberedBackup reads the directory and matches names by hand rather
// than globbing for them: the path being backed up may itself contain glob
// metacharacters ("report[1].pdf"), which filepath.Glob would interpret.
func highestNumberedBackup(path string) (int, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	highest := 0
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), base+".~")
		if !ok {
			continue
		}
		num, ok := strings.CutSuffix(rest, "~")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(num); err == nil && n > highest {
			highest = n
		}
	}
	return highest, nil
}
