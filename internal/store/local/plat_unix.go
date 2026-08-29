//go:build linux || darwin

package local

import (
	"errors"
	"io/fs"
	"syscall"

	"golang.org/x/sys/unix"
)

// The stat fields and the xattr calls are the same on Linux and macOS apart
// from spelling; everything that can be shared lives here, and the files
// beside this one keep only what genuinely differs — the reflink mechanism,
// the in-kernel copy, and the names of the timestamp fields.

// copyXattrs mirrors user-visible extended attributes. Attributes the caller is
// not privileged to set (security.*, trusted.*) are skipped rather than
// treated as failures, matching cp --preserve=xattr.
func copyXattrs(srcPath, dstPath string) error {
	size, err := unix.Listxattr(srcPath, nil)
	if err != nil || size == 0 {
		if xattrsUnsupported(err) {
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
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) ||
				xattrsUnsupported(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// xattrsUnsupported recognises a filesystem that has no extended attributes at
// all. Linux says EOPNOTSUPP where macOS says ENOTSUP; on Linux the two are
// the same number, on macOS they are not, so both are checked.
func xattrsUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
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
