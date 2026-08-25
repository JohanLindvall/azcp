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
		want := size - total
		if want > chunk {
			want = chunk
		}
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

// copyXattrs mirrors user-visible extended attributes. Attributes the caller is
// not privileged to set (security.*, trusted.*) are skipped rather than
// treated as failures, matching cp --preserve=xattr.
func copyXattrs(srcPath, dstPath string) error {
	size, err := unix.Listxattr(srcPath, nil)
	if err != nil || size == 0 {
		if errors.Is(err, unix.EOPNOTSUPP) {
			return nil
		}
		return err
	}
	buf := make([]byte, size)
	size, err = unix.Listxattr(srcPath, buf)
	if err != nil {
		return err
	}
	var firstErr error
	for _, name := range splitNull(buf[:size]) {
		vlen, err := unix.Getxattr(srcPath, name, nil)
		if err != nil {
			continue
		}
		val := make([]byte, vlen)
		if vlen > 0 {
			if _, err := unix.Getxattr(srcPath, name, val); err != nil {
				continue
			}
		}
		if err := unix.Setxattr(dstPath, name, val, 0); err != nil {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EOPNOTSUPP) ||
				errors.Is(err, unix.EACCES) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func splitNull(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func allocatedBytes(fi fs.FileInfo) (int64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Blocks * 512, true
}

func accessTimeOf(fi fs.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.ModTime()
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}

func ownerOf(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, k := fi.Sys().(*syscall.Stat_t)
	if !k {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

func deviceOfSys(sys any) (uint64, bool) {
	st, ok := sys.(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

func fileIdentity(fi fs.FileInfo) (FileID, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, 0, false
	}
	return FileID{uint64(st.Dev), st.Ino}, int(st.Nlink), true
}
