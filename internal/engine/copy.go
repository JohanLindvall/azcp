package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
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
func (e *Engine) transfer(ctx context.Context, t *task, pt *progress.Task) (retErr error) {
	if t.noop {
		return nil
	}
	if t.backup != "" {
		backup := t.backup
		if err := os.Rename(t.dst.Path, backup); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("cannot back up %s: %w", quote(t.dst.Display()), err)
			}
		} else {
			defer func() {
				if retErr == nil {
					return
				}
				// Like cp, put the original name back when the copy failed
				// before creating a replacement, for example on a source open.
				// A partial destination keeps its backup available separately.
				if _, err := os.Lstat(t.dst.Path); os.IsNotExist(err) {
					if err := os.Rename(backup, t.dst.Path); err != nil {
						e.log.Warn("cannot restore backup", "path", t.dst.Display(), "backup", backup, "error", err)
					} else {
						t.backup = backup
					}
				}
			}()
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
			if err := e.replace(t, func() error {
				return os.Symlink(p.SymlinkDest, t.dst.Path)
			}); err != nil {
				return err
			}
			e.restoreAttrs(t)
			return nil
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
	opts := e.transferOptions()
	opts.Progress = pt.Set
	opts.Metadata = e.uploadMetadata(t.src)
	if e.opt.AttributesOnly {
		// The attributes, not the data: an existing blob keeps its content and
		// has its metadata and headers replaced. Only when nothing is there
		// does this degenerate to creating an empty blob, as cp creates an
		// empty file.
		return e.az.PutAttrs(ctx, t.dst, opts)
	}

	// A symbolic link has no content beyond where it points, so it is stored
	// as an empty blob whose metadata says what it is.
	if t.src.IsSymlink() {
		return e.az.PutMarker(ctx, t.dst, opts)
	}
	compress := e.compresses(t.src)
	if compress {
		// The headers describe the file inside: its own type, and the coding
		// it arrives in. Guessed from the blob's name they would say "a gzip
		// file", which is what the encoding header exists to avoid.
		opts.ContentEncoding = e.opt.Compress.Format.String()
		opts.ContentType = cmp.Or(e.opt.ContentType, azure.ContentTypeFor(t.src.Name()),
			"application/octet-stream")
	}
	if t.src.IsPipe() {
		return e.uploadPipe(ctx, t, pt, opts, compress)
	}
	if compress {
		return e.az.UploadEncoded(ctx,
			func() io.ReadCloser { return e.encoded(ctx, t.src.URL.Path, pt) },
			t.src.Size, t.dst, opts)
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
		// The scanner deliberately admits unfinished downloads even without
		// --resume; they must be restarted, not refused as completed files.
		if !azure.IncompleteDownload(t.dst.Path) {
			flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		}
	}
	f, err := e.openDest(t, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	decodeStream := e.opt.Decompress && !e.opt.Resume && decompressible(t.src.ContentEncoding)
	if !e.opt.AttributesOnly && (!info.Mode().IsRegular() || decodeStream) {
		if err := e.downloadStream(ctx, t, f, pt); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return writeError(t, err)
		}
		if info.Mode().IsRegular() {
			if err := azure.RemoveResumeRecord(t.dst.Path); err != nil {
				return err
			}
		}
		e.restoreAttrs(t)
		return nil
	}

	if !e.opt.AttributesOnly {
		opts := e.transferOptions()
		opts.Progress = pt.Set
		opts.KeepResumeRecord = e.opt.Resume && e.opt.Decompress && decompressible(t.src.ContentEncoding)
		if err := e.az.Download(ctx, t.src, f, opts); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %w", quote(t.dst.Display()), err)
	}
	if e.opt.Decompress && !e.opt.AttributesOnly && decompressible(t.src.ContentEncoding) {
		if e.opt.Resume {
			// Once expansion starts these compressed ranges must never be
			// trusted again, even if rewriting the destination is interrupted.
			if err := azure.ResetResumeRecord(t.dst.Path); err != nil {
				return err
			}
		}
		_, derr := decompressTo(ctx, t.dst.Path, t.src.ContentEncoding, t.dst.Path)
		if derr != nil {
			return derr
		}
		if e.opt.Resume {
			if err := os.Remove(t.dst.Path + azure.ResumeSuffix); err != nil && !os.IsNotExist(err) {
				return err
			}
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
	opts.ContentType = cmp.Or(opts.ContentType, t.src.ContentType)
	opts.ContentEncoding = cmp.Or(opts.ContentEncoding, t.src.ContentEncoding)
	opts.ContentDisposition = cmp.Or(opts.ContentDisposition, t.src.ContentDisposition)
	opts.ContentLanguage = cmp.Or(opts.ContentLanguage, t.src.ContentLanguage)
	opts.CacheControl = cmp.Or(opts.CacheControl, t.src.CacheControl)
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
	if e.opt.AttributesOnly {
		return e.az.PutAttrs(ctx, t.dst, opts)
	}
	return e.az.Copy(ctx, t.src, t.dst, opts)
}

// copyLocal handles the filesystem-to-filesystem case, including the link and
// attribute-only variants cp offers.
func (e *Engine) copyLocal(ctx context.Context, t *task, pt *progress.Task) (retErr error) {
	srcPath, dstPath := t.src.URL.Path, t.dst.Path

	switch {
	case e.opt.SymbolicLink:
		err := e.replace(t, func() error {
			if !filepath.IsAbs(srcPath) {
				cwd, err := os.Stat(".")
				if err != nil {
					return err
				}
				parent, err := os.Stat(filepath.Dir(dstPath))
				if err != nil {
					return err
				}
				if !os.SameFile(cwd, parent) {
					return plainf("%s: can make relative symbolic links only in current directory", t.dst.Display())
				}
			}
			return os.Symlink(srcPath, dstPath)
		})
		return linkError(t, "symbolic", err)
	case e.opt.HardLink:
		linkPath := srcPath
		if !t.src.IsSymlink() {
			info, err := os.Lstat(srcPath)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				linkPath, err = filepath.EvalSymlinks(srcPath)
				if err != nil {
					return err
				}
			}
		}
		return linkError(t, "hard", e.replace(t, func() error { return os.Link(linkPath, dstPath) }))
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

	switch {
	case e.opt.AttributesOnly:
		f, err := e.openDest(t, os.O_WRONLY|os.O_CREATE, e.opt.CreationMode(t.src.Mode))
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	case e.compresses(t.src):
		if err := e.compressLocal(ctx, t, pt); err != nil {
			return err
		}
	default:
		opts := local.CopyOptions{
			Reflink:  e.opt.Reflink,
			Sparse:   e.opt.Sparse,
			Mode:     e.opt.CreationMode(t.src.Mode),
			Progress: pt.Add,
			Excl:     e.opt.NoClobber && !t.followDangling,
		}
		written, err := local.CopyFile(ctx, srcPath, dstPath, opts)
		if err != nil {
			if !e.forceRetryable(err, dstPath) {
				return destError(t, err)
			}
			// -f: the destination exists but cannot be opened for writing.
			// Remove it and try once more, which is what cp does.
			e.log.Info("removing unwritable destination and retrying",
				"path", dstPath, "error", err)
			if rmErr := os.Remove(dstPath); rmErr != nil {
				return err
			}
			if written, err = local.CopyFile(ctx, srcPath, dstPath, opts); err != nil {
				return destError(t, err)
			}
		}
		pt.SetSize(written)
		if t.src.IsPipe() {
			// Only now is there a length to report.
			t.src.Size = written
		}
	}

	e.applyAttrs(t)
	return nil
}

func linkError(t *task, kind string, err error) error {
	if err == nil {
		return nil
	}
	var plain *plainError
	if errors.As(err, &plain) {
		return err
	}
	cause := err
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		cause = linkErr.Err
	}
	return &plainError{msg: fmt.Sprintf("cannot create %s link %s to %s: %s",
		kind, quote(t.dst.Display()), quote(t.src.URL.Display()), sentence(cause)), cause: err}
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
	info, statErr := sourceInfo(t.src)
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
	if !errors.Is(err, os.ErrExist) || e.opt.NoClobber ||
		((e.opt.AttributesOnly || (e.opt.HardLink && !t.src.IsSymlink() && !e.opt.Interactive) || e.opt.SymbolicLink) && !e.opt.Force) {
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
	if t.followDangling {
		flags &^= os.O_EXCL
	}
	f, err := os.OpenFile(t.dst.Path, flags, mode)
	if err == nil {
		return f, nil
	}
	if e.forceRetryable(err, t.dst.Path) {
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
	if errors.Is(err, local.ErrSameFile) {
		return plainf("%s and %s are the same file", quote(t.src.URL.Display()), quote(t.dst.Display()))
	}
	if errors.Is(err, syscall.EISDIR) {
		return plainf("cannot overwrite directory %s with non-directory",
			quote(t.dst.Display()))
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "open" {
		switch pathErr.Path {
		case t.src.URL.Path:
			return &plainError{msg: fmt.Sprintf("cannot open %s for reading: %s", quote(t.src.URL.Display()), sentence(pathErr.Err)), cause: err}
		case t.dst.Path:
			return &plainError{msg: fmt.Sprintf("cannot create regular file %s: %s", quote(t.dst.Display()), sentence(pathErr.Err)), cause: err}
		}
	}
	if phrased, ok := ioError(t, err); ok {
		return phrased
	}
	return fmt.Errorf("cannot create %s: %w", quote(t.dst.Display()), err)
}

// ioError phrases a failure to read the source or to write or close the
// destination the way cp does — "error writing '/dev/stdout': Broken pipe".
// It reports false for anything else.
func ioError(t *task, err error) (error, bool) {
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return nil, false
	}
	var msg string
	switch {
	case pathErr.Op == "write" && pathErr.Path == t.dst.Path:
		msg = "error writing " + quote(t.dst.Display())
	case pathErr.Op == "read" && pathErr.Path == t.src.URL.Path:
		msg = "error reading " + quote(t.src.URL.Display())
	case pathErr.Op == "close" && pathErr.Path == t.dst.Path:
		msg = "failed to close " + quote(t.dst.Display())
	default:
		return nil, false
	}
	return &plainError{msg: msg + ": " + sentence(pathErr.Err), cause: err}, true
}

// writeError is ioError for a caller with nothing better to say otherwise.
func writeError(t *task, err error) error {
	if phrased, ok := ioError(t, err); ok {
		return phrased
	}
	return err
}

// sentence capitalises an error the way strerror reads.
func sentence(err error) string {
	cause := err.Error()
	if cause != "" {
		cause = strings.ToUpper(cause[:1]) + cause[1:]
	}
	return cause
}

// forceRetryable reports whether -f should clear the destination and try again.
func (e *Engine) forceRetryable(err error, dst string) bool {
	if !e.opt.Force || e.opt.NoClobber {
		return false
	}
	// CopyFile can fail while opening the source or after writing has begun.
	// Neither permits removing the destination: -f only retries its open.
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "open" || pathErr.Path != dst {
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
	info, err := sourceAttrs(t.src)
	if err != nil {
		e.log.Warn("cannot read source attributes",
			"path", t.src.URL.Display(), "error", err)
		return
	}
	errs := local.ApplyAttrs(t.src.URL.Path, t.dst.Path, info, e.opt.Preserve, t.src.IsSymlink() || e.opt.SymbolicLink)
	for _, err := range errs {
		e.log.Warn("cannot preserve attribute",
			"path", t.dst.Display(), "error", err)
	}
}

// checkUnsupported rejects combinations that cannot work against blob storage
// before any data moves, rather than failing partway through.
func (e *Engine) checkUnsupported(dest *uri.URL) error {
	if e.opt.CopyContents {
		e.log.Warn("ignoring --copy-contents: a recursive copy skips special files rather than reading them")
	}
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
	if e.opt.Compress.On() && srcRemote {
		return errors.New("--compress applies to local sources only; " +
			"a blob already in storage is copied as it is")
	}
	if !remote {
		return nil
	}
	switch {
	case e.opt.HardLink:
		return errors.New("--link is not possible with blob storage")
	case e.opt.SymbolicLink:
		return errors.New("--symbolic-link is not possible with blob storage")
	case e.opt.Backup != cli.BackupNone && dest.IsRemote():
		return errors.New("--backup is not supported for blob destinations")
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
	return "", errors.New("no backup requested")
}
