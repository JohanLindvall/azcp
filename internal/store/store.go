// Package store abstracts the two namespaces the tool copies between: the
// local filesystem and Azure Blob Storage. Only the naming operations (stat,
// list, walk) are behind the interface. The bulk data paths are dispatched
// concretely by the engine, because each pairing has its own fast route:
// parallel block upload, parallel ranged download, or a server-side copy.
package store

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/JohanLindvall/azcp/internal/uri"
)

// Kind classifies a node.
type Kind int

const (
	KindFile Kind = iota
	KindDir
	KindSymlink
	KindOther // fifo, socket, device — copied only with --force or skipped
)

func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "directory"
	case KindSymlink:
		return "symlink"
	default:
		return "special file"
	}
}

// Node is a single entry in either namespace.
type Node struct {
	URL     *uri.URL
	Kind    Kind
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time

	// LinkTarget is set for symlinks.
	LinkTarget string

	// Blob-only metadata.
	ContentType string
	ETag        string
	MD5         []byte
	AccessTier  string

	// Sys carries the platform-specific stat result for local files, used for
	// ownership, timestamps and hard-link detection.
	Sys any
}

func (n *Node) IsDir() bool     { return n != nil && n.Kind == KindDir }
func (n *Node) IsRegular() bool { return n != nil && n.Kind == KindFile }
func (n *Node) IsSymlink() bool { return n != nil && n.Kind == KindSymlink }

// Name returns the node's last path element.
func (n *Node) Name() string { return n.URL.Base() }

// ErrNotExist is returned by Stat when nothing lives at the location. Callers
// test it with errors.Is, and both stores wrap their native errors so that
// works.
var ErrNotExist = fs.ErrNotExist

// IsNotExist is a convenience wrapper.
func IsNotExist(err error) bool { return errors.Is(err, ErrNotExist) }

// Store is the naming half of a namespace.
type Store interface {
	// Scheme identifies the namespace, matching uri.Scheme* constants.
	Scheme() string

	// Stat describes the node at u. When follow is true, symlinks are
	// resolved. It returns an error wrapping ErrNotExist when nothing is there.
	Stat(ctx context.Context, u *uri.URL, follow bool) (*Node, error)

	// ReadDir lists the immediate children of a directory or blob prefix.
	// Entries are returned in lexical order.
	ReadDir(ctx context.Context, u *uri.URL) ([]*Node, error)

	// WalkAll visits every node beneath u, recursively, in no guaranteed
	// order. Errors reading individual subtrees are reported to onError and do
	// not abort the walk; returning a non-nil error from onError does.
	WalkAll(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*Node) error) error

	// MkdirAll makes u usable as a destination directory or prefix.
	MkdirAll(ctx context.Context, u *uri.URL, mode fs.FileMode) error

	// Remove deletes a single node.
	Remove(ctx context.Context, u *uri.URL) error
}
