//go:build linux

package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// tryReflink asks the filesystem to share extents between the two files
// (btrfs, XFS with reflink, bcachefs). A failure here is ordinary: most
// filesystems cannot do it.
func tryReflink(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}

// kernelCopy moves data with copy_file_range, which keeps the bytes inside the
// kernel. It reports ok=false when the platform or filesystem refuses, so the
// caller can fall back to a read/write loop.
func kernelCopy(ctx context.Context, dst, src *os.File, size int64, opts *CopyOptions) (int64, bool, error) {
	if size == 0 {
		return 0, true, nil
	}
	// Cap each call so progress stays responsive on very large files.
	const chunk = 64 << 20
	var total int64
	for total < size {
		if err := ctx.Err(); err != nil {
			return total, true, err
		}
		want := min(size-total, chunk)
		n, err := unix.CopyFileRange(int(src.Fd()), nil, int(dst.Fd()), nil, int(want), 0)
		if err != nil {
			if total == 0 && isCopyRangeUnsupported(err) {
				return 0, false, nil
			}
			return total, true, err
		}
		if n == 0 {
			// Short source: nothing left to move.
			break
		}
		total += int64(n)
		opts.report(int64(n))
	}
	return total, true, nil
}

// isCopyRangeUnsupported distinguishes "this kernel or filesystem will not do
// it" from a genuine I/O failure.
func isCopyRangeUnsupported(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case unix.ENOSYS, unix.EXDEV, unix.EINVAL, unix.EOPNOTSUPP, unix.EPERM, unix.ETXTBSY, unix.EBADF:
		return true
	}
	return false
}

// lutimes sets timestamps without following a symlink.
func lutimes(path string, atime, mtime time.Time) error {
	ts := []unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(mtime.UnixNano()),
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func accessTimeOf(fi fs.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.ModTime()
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}
