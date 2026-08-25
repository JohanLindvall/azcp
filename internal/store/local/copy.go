package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Reflink selects whether to attempt a copy-on-write clone.
type Reflink int

const (
	ReflinkAuto   Reflink = iota // try, fall back to a data copy
	ReflinkNever                 // always copy data
	ReflinkAlways                // fail if the filesystem cannot clone
)

// Sparse selects hole handling.
type Sparse int

const (
	SparseAuto   Sparse = iota // preserve holes when the source is sparse
	SparseNever                // write every byte, holes included
	SparseAlways               // punch a hole for every run of zeros
)

// CopyOptions configures a single file copy.
type CopyOptions struct {
	Reflink  Reflink
	Sparse   Sparse
	Mode     fs.FileMode
	BufSize  int
	Progress func(n int64)
	// Excl makes the destination fail rather than truncate if it already
	// exists, which is how cp -n avoids a create/clobber race.
	Excl bool
}

const defaultBufSize = 512 << 10

func (o *CopyOptions) report(n int64) {
	if o.Progress != nil && n > 0 {
		o.Progress(n)
	}
}

// CopyFile copies regular file contents from srcPath to dstPath. It returns the
// number of bytes written.
func CopyFile(ctx context.Context, srcPath, dstPath string, opts CopyOptions) (int64, error) {
	sf, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer sf.Close()

	si, err := sf.Stat()
	if err != nil {
		return 0, err
	}
	size := si.Size()

	mode := opts.Mode
	if mode == 0 {
		mode = si.Mode().Perm()
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if opts.Excl {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	df, err := os.OpenFile(dstPath, flags, mode)
	if err != nil {
		return 0, err
	}
	// Close errors matter: a deferred close can hide a failed writeback, so the
	// close result is folded into the returned error below.
	closed := false
	defer func() {
		if !closed {
			df.Close()
		}
	}()

	if opts.Reflink != ReflinkNever {
		switch err := tryReflink(df, sf); {
		case err == nil:
			opts.report(size)
			closed = true
			return size, df.Close()
		case opts.Reflink == ReflinkAlways:
			return 0, fmt.Errorf("failed to clone %q: %w", srcPath, err)
		}
	}

	sparse := opts.Sparse == SparseAlways ||
		(opts.Sparse == SparseAuto && looksSparse(si, size))

	var written int64
	if sparse {
		written, err = copySparse(ctx, df, sf, size, &opts)
	} else {
		written, err = copyDense(ctx, df, sf, size, &opts)
	}
	if err != nil {
		return written, err
	}
	closed = true
	return written, df.Close()
}

// looksSparse uses the same test cp does: fewer allocated bytes than the
// apparent size means the source has holes worth preserving.
func looksSparse(fi fs.FileInfo, size int64) bool {
	alloc, ok := allocatedBytes(fi)
	return ok && alloc < size
}

// copyDense copies every byte, preferring an in-kernel copy when the platform
// offers one and falling back to a buffered loop.
func copyDense(ctx context.Context, dst, src *os.File, size int64, opts *CopyOptions) (int64, error) {
	if n, ok, err := kernelCopy(ctx, dst, src, size, opts); ok {
		return n, err
	}
	buf := make([]byte, bufSize(opts))
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			opts.report(int64(nw))
			if werr != nil {
				return total, werr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}

// copySparse writes only the non-zero runs and seeks over the rest, then sets
// the file length so trailing holes survive.
func copySparse(ctx context.Context, dst, src *os.File, size int64, opts *CopyOptions) (int64, error) {
	buf := make([]byte, bufSize(opts))
	var off, total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			chunk := buf[:nr]
			if isAllZero(chunk) {
				// Leave a hole: the file is extended by the final Truncate.
				off += int64(nr)
				total += int64(nr)
				opts.report(int64(nr))
			} else {
				if _, err := dst.Seek(off, io.SeekStart); err != nil {
					return total, err
				}
				nw, werr := dst.Write(chunk)
				off += int64(nw)
				total += int64(nw)
				opts.report(int64(nw))
				if werr != nil {
					return total, werr
				}
				if nw != nr {
					return total, io.ErrShortWrite
				}
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return total, rerr
		}
	}
	if err := dst.Truncate(size); err != nil {
		return total, err
	}
	return total, nil
}

func bufSize(opts *CopyOptions) int {
	if opts.BufSize > 0 {
		return opts.BufSize
	}
	return defaultBufSize
}

func isAllZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// CopySymlink recreates a symbolic link at dstPath pointing at the same target.
func CopySymlink(srcPath, dstPath string) error {
	target, err := os.Readlink(srcPath)
	if err != nil {
		return err
	}
	if err := os.Symlink(target, dstPath); err != nil {
		return err
	}
	return nil
}

// Preserve records which attributes to carry across, mirroring cp's
// --preserve list.
type Preserve struct {
	Mode       bool
	Ownership  bool
	Timestamps bool
	Links      bool
	XAttr      bool
	Context    bool
}

// Any reports whether anything at all is preserved.
func (p Preserve) Any() bool {
	return p.Mode || p.Ownership || p.Timestamps || p.Links || p.XAttr || p.Context
}

// ApplyAttrs copies the requested attributes from a source stat onto dstPath.
// It returns the problems it hit rather than stopping at the first: failing to
// chown as a non-root user should not prevent timestamps being set.
func ApplyAttrs(srcPath, dstPath string, fi fs.FileInfo, p Preserve, isSymlink bool) []error {
	var errs []error

	if p.Ownership {
		if uid, gid, ok := ownerOf(fi); ok {
			if err := os.Lchown(dstPath, uid, gid); err != nil {
				errs = append(errs, fmt.Errorf("preserve ownership: %w", err))
			}
		}
	}
	// Mode and xattrs are meaningless on a symlink itself, and chmod would
	// follow the link and alter the target.
	if !isSymlink {
		if p.Mode {
			if err := os.Chmod(dstPath, fi.Mode().Perm()|specialBits(fi.Mode())); err != nil {
				errs = append(errs, fmt.Errorf("preserve mode: %w", err))
			}
		}
		if p.XAttr {
			if err := copyXattrs(srcPath, dstPath); err != nil {
				errs = append(errs, fmt.Errorf("preserve xattrs: %w", err))
			}
		}
	}
	if p.Timestamps {
		if err := lutimes(dstPath, accessTimeOf(fi), fi.ModTime()); err != nil {
			errs = append(errs, fmt.Errorf("preserve timestamps: %w", err))
		}
	}
	return errs
}

func specialBits(m fs.FileMode) fs.FileMode {
	var out fs.FileMode
	if m&fs.ModeSetuid != 0 {
		out |= fs.ModeSetuid
	}
	if m&fs.ModeSetgid != 0 {
		out |= fs.ModeSetgid
	}
	if m&fs.ModeSticky != 0 {
		out |= fs.ModeSticky
	}
	return out
}

// FileID identifies a file uniquely on a filesystem; the engine uses it to
// recreate hard links between copied files (cp --preserve=links).
type FileID struct{ Dev, Ino uint64 }

// IDOf returns the file identity from a stat result, along with its link
// count, so only multiply-linked files need tracking.
func IDOf(fi fs.FileInfo) (FileID, int, bool) { return fileIdentity(fi) }

// DeviceOf returns the filesystem a node lives on, taken from the raw stat
// result carried on a Node. It is how --one-file-system recognises a mount
// point.
func DeviceOf(sys any) (uint64, bool) { return deviceOfSys(sys) }
