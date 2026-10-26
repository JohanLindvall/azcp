// SPDX-License-Identifier: MIT

//go:build !windows

package selfupdate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// An OS lock is released even after SIGKILL. The lock file must keep its name:
// unlinking it would let a third process lock a different inode at that name.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func replaceExecutable(target, staged string) (string, error) {
	return "", os.Rename(staged, target)
}

func cleanupBackup(_, _ string) bool { return true }
