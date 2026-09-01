// Package local implements the store interface over the local filesystem.
package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Store is the local filesystem namespace.
type Store struct {
	log *slog.Logger
	// Follow makes directory traversal resolve symbolic links, matching cp -L.
	// Traversal keeps a set of visited inodes so a link cycle cannot loop.
	Follow bool
	// OneFileSystem stops traversal at mount-point boundaries (cp -x).
	OneFileSystem bool
}

// New returns a local store.
func New(log *slog.Logger, follow bool) *Store {
	return &Store{log: log, Follow: follow}
}

// Scheme identifies the namespace, matching uri.SchemeFile.
func (s *Store) Scheme() string { return uri.SchemeFile }

// Stat describes the node at u.
func (s *Store) Stat(_ context.Context, u *uri.URL, follow bool) (*store.Node, error) {
	p := u.Path
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, wrap(p, err)
	}
	if follow && fi.Mode()&fs.ModeSymlink != 0 {
		target, terr := os.Stat(p)
		if terr != nil {
			// A dangling link: report it as it is, so the caller can decide
			// whether that is an error (cp -L) or fine (cp -P).
			return s.node(u, fi), nil
		}
		fi = target
	}
	return s.node(u, fi), nil
}

func (s *Store) node(u *uri.URL, fi fs.FileInfo) *store.Node {
	n := &store.Node{
		URL:     u,
		Size:    fi.Size(),
		Mode:    fi.Mode(),
		ModTime: fi.ModTime(),
		Sys:     fi.Sys(),
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		n.Kind = store.KindSymlink
		if t, err := os.Readlink(u.Path); err == nil {
			n.LinkTarget = t
		}
		// Resolve the target's mode so callers can ask whether the link points
		// at a directory without statting again.
		if ti, err := os.Stat(u.Path); err == nil {
			n.Mode = fi.Mode() | (ti.Mode() & fs.ModeDir)
		}
	case fi.IsDir():
		n.Kind = store.KindDir
	case fi.Mode().IsRegular():
		n.Kind = store.KindFile
	default:
		n.Kind = store.KindOther
	}
	return n
}

// ReadDir lists the immediate children of a directory, in lexical order.
func (s *Store) ReadDir(_ context.Context, u *uri.URL) ([]*store.Node, error) {
	entries, err := os.ReadDir(u.Path)
	if err != nil {
		return nil, wrap(u.Path, err)
	}
	out := make([]*store.Node, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			// The entry vanished between listing and stat; that is normal on a
			// live filesystem and not worth failing the whole walk over.
			s.log.Debug("entry disappeared during listing",
				"path", filepath.Join(u.Path, e.Name()), "error", err)
			continue
		}
		out = append(out, s.node(u.Join(e.Name()), fi))
	}
	// os.ReadDir has already sorted the entries by name.
	return out, nil
}

// WalkAll visits every node beneath u. Unreadable subtrees are reported to
// onError and skipped.
func (s *Store) WalkAll(ctx context.Context, u *uri.URL,
	onError func(*uri.URL, error) error, fn func(*store.Node) error) error {

	root, err := s.Stat(ctx, u, true)
	if err != nil {
		return err
	}
	rootDev, _ := deviceOfSys(root.Sys)
	visited := map[FileID]bool{}
	if s.Follow {
		// The root belongs in the visited set from the start, or a link
		// pointing back at it re-enters the tree and walks it all twice.
		if info, err := os.Stat(u.Path); err == nil {
			if id, _, ok := fileIdentity(u.Path, info); ok {
				visited[id] = true
			}
		}
	}
	return s.walk(ctx, u, onError, fn, visited, rootDev, 0)
}

// maxWalkDepth is a backstop against a symlink cycle that inode tracking
// somehow fails to catch (for example across filesystems that reuse inodes).
const maxWalkDepth = 256

func (s *Store) walk(ctx context.Context, dir *uri.URL,
	onError func(*uri.URL, error) error, fn func(*store.Node) error,
	visited map[FileID]bool, rootDev uint64, depth int) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > maxWalkDepth {
		return onError(dir, fmt.Errorf("directory nesting exceeds %d levels", maxWalkDepth))
	}
	entries, err := s.ReadDir(ctx, dir)
	if err != nil {
		return onError(dir, err)
	}
	for _, e := range entries {
		if err := fn(e); err != nil {
			return err
		}
		descend := e.IsDir() || (s.Follow && e.IsSymlink() && e.Mode.IsDir())
		if !descend {
			continue
		}
		// Ownership of the inode set is what makes following links safe.
		if s.Follow {
			target, terr := os.Stat(e.URL.Path)
			if terr != nil {
				if oerr := onError(e.URL, terr); oerr != nil {
					return oerr
				}
				continue
			}
			if id, _, ok := fileIdentity(e.URL.Path, target); ok {
				if visited[id] {
					s.log.Warn("skipping symlink loop", "path", e.URL.Display())
					continue
				}
				visited[id] = true
			}
		}
		if s.OneFileSystem {
			if dev, ok := deviceOfSys(e.Sys); ok && dev != rootDev {
				s.log.Debug("skipping other filesystem", "path", e.URL.Display())
				continue
			}
		}
		if err := s.walk(ctx, e.URL, onError, fn, visited, rootDev, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// MkdirAll creates a directory and any missing parents.
func (s *Store) MkdirAll(_ context.Context, u *uri.URL, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o777
	}
	if err := os.MkdirAll(u.Path, mode); err != nil {
		return wrap(u.Path, err)
	}
	return nil
}

// Remove deletes a single file, symlink or empty directory.
func (s *Store) Remove(_ context.Context, u *uri.URL) error {
	if err := os.Remove(u.Path); err != nil {
		return wrap(u.Path, err)
	}
	return nil
}

// wrap normalises errors so callers can use store.IsNotExist. The os package
// already reports most failures as a PathError, which unwraps to the errno
// IsNotExist looks for; a LinkError is reshaped to match, and anything else is
// given the path it was about.
func wrap(path string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return &os.PathError{Op: le.Op, Path: le.Old, Err: le.Err}
	}
	return &os.PathError{Op: "stat", Path: path, Err: err}
}
