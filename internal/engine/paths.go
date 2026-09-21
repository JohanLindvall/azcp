package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/store"
)

// resolvedPath follows the existing part of a path before appending any new
// suffix. Comparing operand strings misses absolute aliases and destinations
// reached through a symbolic link into the source tree.
func resolvedPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		if filepath.IsAbs(resolved) {
			return resolved, nil
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		// EvalSymlinks leaves relative paths relative. The working directory
		// added by Abs can itself contain aliases, including Windows 8.3 names.
		return filepath.EvalSymlinks(resolved)
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolvedPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

// Blob keys are names, not filesystem paths. Reject components that the local
// platform would clean away or reinterpret before joining a remote tree onto
// a local destination.
func checkLocalName(rel string) error {
	if rel == "." || !fs.ValidPath(rel) || !filepath.IsLocal(filepath.FromSlash(rel)) ||
		(runtime.GOOS == "windows" && strings.ContainsAny(rel, `\:`)) {
		return plainf("cannot copy blob path %s to the local filesystem", quote(rel))
	}
	return nil
}

// sourceInfo follows exactly the links the planner resolved. Lstat here would
// preserve the link's mode and timestamp on a copy of its target.
func sourceInfo(src *store.Node) (os.FileInfo, error) {
	if src.IsSymlink() {
		return os.Lstat(src.URL.Path)
	}
	return os.Stat(src.URL.Path)
}

// prepareLocalTask checks before a backup or unlink can remove the source.
// The low-level copier repeats the identity check on open handles before
// truncation, since paths can change between planning and execution.
func (e *Engine) prepareLocalTask(t *task) error {
	if t.dst.IsRemote() {
		return nil
	}
	var si os.FileInfo
	if !t.src.URL.IsRemote() {
		var err error
		si, err = sourceInfo(t.src)
		if err != nil {
			return err
		}
		if t.backup != "" {
			if backup, err := os.Stat(t.backup); err == nil && os.SameFile(si, backup) {
				return plainf("backing up %s might destroy source;  %s not copied", quote(t.dst.Display()), quote(t.src.URL.Display()))
			}
		}
	}
	di, err := os.Lstat(t.dst.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if di.IsDir() {
		return plainf("cannot overwrite directory %s with non-directory", quote(t.dst.Display()))
	}
	if !t.src.IsSymlink() {
		if di.Mode()&os.ModeSymlink != 0 {
			target, err := os.Stat(t.dst.Path)
			_, posix := os.LookupEnv("POSIXLY_CORRECT")
			replacesLink := t.backup != "" || t.removeFirst || e.opt.HardLink || e.opt.SymbolicLink ||
				(t.src.URL.IsRemote() && store.DecodePosixMeta(t.src.Metadata).IsSymlink())
			if os.IsNotExist(err) && !posix && !replacesLink {
				return plainf("not writing through dangling symlink %s", quote(t.dst.Display()))
			}
			if err == nil {
				di = target
			}
		}
	}
	if t.src.URL.IsRemote() {
		return nil
	}
	if !os.SameFile(si, di) {
		return nil
	}
	if e.opt.HardLink && !t.src.IsSymlink() {
		if di, err := os.Lstat(t.dst.Path); err == nil && di.Mode()&os.ModeSymlink == 0 {
			t.noop = true
			return nil
		}
	}
	sp, err := resolvedPath(filepath.Dir(t.src.URL.Path))
	if err != nil {
		return err
	}
	dp, err := resolvedPath(filepath.Dir(t.dst.Path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(sp, dp)
	sameBase := filepath.Base(t.src.URL.Path) == filepath.Base(t.dst.Path)
	if runtime.GOOS == "windows" {
		sameBase = strings.EqualFold(filepath.Base(t.src.URL.Path), filepath.Base(t.dst.Path))
	}
	sameName := err == nil && rel == "." && sameBase
	if !sameName && (t.backup != "" || t.removeFirst || t.src.IsSymlink()) {
		return nil
	}
	if sameName && e.opt.Force && e.opt.Backup != cli.BackupNone && si.Mode().IsRegular() && t.backup != t.dst.Path {
		// cp -bf FILE FILE copies onto the backup name, leaving FILE in place.
		t.dst = t.dst.WithPathPart(t.backup)
		t.backup = ""
		t.removeFirst = false
		return nil
	}
	return plainf("%s and %s are the same file", quote(t.src.URL.Display()), quote(t.dst.Display()))
}
