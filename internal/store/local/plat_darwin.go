//go:build darwin

package local

import (
	"context"
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

func accessTimeOf(fi fs.FileInfo) time.Time {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.ModTime()
	}
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
}
