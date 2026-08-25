//go:build windows

package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Windows has none of the POSIX primitives the copy path uses, but it does have
// a file identity, and that one matters more than the rest: without it there is
// no way to notice that --dereference has walked into a directory it has
// already been in, and a symbolic link pointing back up its own tree makes the
// copy recurse until something gives way.
//
// Unlike Unix, the identity is not in the stat result. The file has to be
// opened for it, which is why fileIdentity takes a path.

var errUnsupported = errors.New("not supported on Windows")

func tryReflink(dst, src *os.File) error { return errUnsupported }

func kernelCopy(context.Context, *os.File, *os.File, int64, *CopyOptions) (int64, bool, error) {
	return 0, false, nil
}

func lutimes(path string, atime, mtime time.Time) error {
	// Windows has no symlink-safe variant; os.Chtimes follows the link, which
	// is the closest available behaviour.
	return os.Chtimes(path, atime, mtime)
}

func copyXattrs(srcPath, dstPath string) error { return nil }

// allocatedBytes has no cheap equivalent here, so sparse files are copied
// densely rather than guessed at.
func allocatedBytes(fs.FileInfo) (int64, bool) { return 0, false }

func accessTimeOf(fi fs.FileInfo) time.Time {
	if d, ok := fi.Sys().(*windows.Win32FileAttributeData); ok {
		return time.Unix(0, d.LastAccessTime.Nanoseconds())
	}
	return fi.ModTime()
}

// ownerOf reports nothing: Windows security descriptors are not uid/gid pairs,
// and pretending otherwise would set the wrong thing.
func ownerOf(fs.FileInfo) (int, int, bool) { return 0, 0, false }

// deviceOfSys reports nothing, so --one-file-system does not apply here. The
// volume serial is available only by opening the file, which is too expensive
// to do for every entry of a walk.
func deviceOfSys(any) (uint64, bool) { return 0, false }

// fileIdentity opens the file to read its volume serial and index, which
// together identify it as an inode number does on Unix.
func fileIdentity(path string, _ fs.FileInfo) (FileID, int, bool) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileID{}, 0, false
	}
	// FILE_FLAG_BACKUP_SEMANTICS is what allows a directory to be opened at
	// all; without it this would work for files and silently fail for the
	// directories the loop guard actually cares about.
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return FileID{}, 0, false
	}
	defer windows.CloseHandle(h)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return FileID{}, 0, false
	}
	return FileID{
		Dev: uint64(info.VolumeSerialNumber),
		Ino: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, int(info.NumberOfLinks), true
}
