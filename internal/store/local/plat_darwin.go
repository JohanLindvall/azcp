//go:build darwin

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

// macOS has the same stat information as Linux under different field names, and
// clonefile in place of the FICLONE ioctl. Without this file the generic
// fallback reports no file identity at all, which quietly disables hard-link
// preservation, --one-file-system, sparse detection and — least obviously and
// most seriously — the loop guard that stops --dereference recursing forever
// through a symbolic link pointing back up its own tree.

// tryReflink asks APFS to clone the file. clonefile needs the destination not
// to exist, so the caller's freshly created file is removed first; on failure
// the caller falls back to copying the data.
func tryReflink(dst, src *os.File) error {
	dstPath := dst.Name()
	if err := os.Remove(dstPath); err != nil {
		return err
	}
	if err := unix.Clonefile(src.Name(), dstPath, 0); err != nil {
		// Put an empty file back so the caller's descriptor still refers to
		// something and the fallback copy behaves as it would have.
		if f, cerr := os.Create(dstPath); cerr == nil {
			f.Close()
		}
		return err
	}
	return nil
}

// kernelCopy has no macOS equivalent that works on arbitrary descriptors;
// reporting false sends the caller to its buffered loop.
func kernelCopy(_ context.Context, _, _ *os.File, _ int64, _ *CopyOptions) (int64, bool, error) {
	return 0, false, nil
}

func lutimes(path string, atime, mtime time.Time) error {
	ts := []unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(mtime.UnixNano()),
	}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// copyXattrs mirrors extended attributes. Attributes the caller may not set are
// skipped rather than treated as failures, as they are on Linux.
func copyXattrs(srcPath, dstPath string) error {
	size, err := unix.Listxattr(srcPath, nil)
	if err != nil || size == 0 {
		if errors.Is(err, unix.ENOTSUP) {
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
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) ||
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
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
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

func fileIdentity(_ string, fi fs.FileInfo) (FileID, int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return FileID{}, 0, false
	}
	return FileID{uint64(st.Dev), st.Ino}, int(st.Nlink), true
}
