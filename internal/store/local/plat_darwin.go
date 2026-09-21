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

// macOS has the same stat information as Linux under different field names.
// Without this file the generic fallback reports no file identity at all,
// which quietly disables hard-link
// preservation, --one-file-system, sparse detection and — least obviously and
// most seriously — the loop guard that stops --dereference recursing forever
// through a symbolic link pointing back up its own tree.

// clonefile creates a new inode; it cannot clone into the open destination.
// Unlinking that destination first makes the fallback write to an unlinked
// inode and breaks existing hard links even when cloning succeeds. Until a
// descriptor-preserving clone is available, use the buffered copy path.
func tryReflink(_, _ *os.File) error {
	return errors.New("reflink into an open destination is not supported on macOS")
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

func accessTimeOf(fi fs.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.ModTime()
	}
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
}
