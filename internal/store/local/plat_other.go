//go:build !linux

package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"
)

var errUnsupported = errors.New("not supported on this platform")

func tryReflink(dst, src *os.File) error { return errUnsupported }

func kernelCopy(context.Context, *os.File, *os.File, int64, *CopyOptions) (int64, bool, error) {
	return 0, false, nil
}

func lutimes(path string, atime, mtime time.Time) error {
	return os.Chtimes(path, atime, mtime)
}

func copyXattrs(srcPath, dstPath string) error { return nil }

func allocatedBytes(fs.FileInfo) (int64, bool) { return 0, false }

func accessTimeOf(fi fs.FileInfo) time.Time { return fi.ModTime() }

func ownerOf(fs.FileInfo) (int, int, bool) { return 0, 0, false }

func deviceOfSys(any) (uint64, bool) { return 0, false }

func fileIdentity(fs.FileInfo) (FileID, int, bool) { return FileID{}, 0, false }
