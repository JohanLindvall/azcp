// SPDX-License-Identifier: MIT

package selfupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, nil
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

func replaceExecutable(target, staged string) (string, error) {
	// Windows permits renaming a running image, but not overwriting it. Keep
	// it under a unique name until the next invocation can remove it.
	dir, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".azcp-old-")
	if err != nil {
		return "", err
	}
	backup := filepath.Join(dir, filepath.Base(target))
	if err := os.Rename(target, backup); err != nil {
		os.Remove(dir)
		return "", err
	}
	if err := os.Rename(staged, target); err != nil {
		if rollback := os.Rename(backup, target); rollback != nil {
			return "", fmt.Errorf("install update: %w; restoring executable failed: %v (original saved at %s)", err, rollback, backup)
		}
		os.Remove(dir)
		return "", err
	}
	if cleanupBackup(target, dir) {
		return "", nil
	}
	return dir, nil
}

func cleanupBackup(target, dir string) bool {
	if filepath.Dir(dir) != filepath.Dir(target) || !strings.HasPrefix(filepath.Base(dir), "."+filepath.Base(target)+".azcp-old-") {
		return false
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if err := os.Remove(filepath.Join(dir, filepath.Base(target))); err != nil && !os.IsNotExist(err) {
		return false
	}
	return os.Remove(dir) == nil
}
