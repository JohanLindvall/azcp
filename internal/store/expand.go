package store

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/JohanLindvall/azcp/internal/glob"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// ExpandOptions tunes pattern expansion.
type ExpandOptions struct {
	// Follow makes symlinks resolve to their target when deciding whether a
	// node is a directory worth descending into. It mirrors cp's -L.
	Follow bool
	Log    *slog.Logger
}

// Expand resolves a location that may contain wildcards into concrete nodes.
// A location with no metacharacters is a single Stat, so ordinary paths cost
// exactly one round trip.
//
// Expansion never fails because a directory could not be listed: unreadable
// subtrees are logged and skipped, the way a shell silently skips them. It
// fails only when the walk itself cannot proceed.
func Expand(ctx context.Context, s Store, u *uri.URL, opts ExpandOptions) ([]*Node, error) {
	pat, err := glob.Compile(u.PathPart())
	if err != nil {
		return nil, err
	}
	if !pat.HasWildcard() {
		n, err := s.Stat(ctx, u, opts.Follow)
		if err != nil {
			return nil, err
		}
		return []*Node{n}, nil
	}

	w := &walker{s: s, base: u, pat: pat, opts: opts, seen: map[string]bool{}}
	prefix, rest := pat.LiteralPrefix()
	start := u.WithPathPart(prefix)
	if !start.IsRemote() && start.Path == "" {
		start = start.WithPathPart(".")
	}
	if err := w.walk(ctx, start, rest); err != nil {
		return nil, err
	}
	slices.SortFunc(w.out, func(a, b *Node) int {
		return strings.Compare(a.URL.PathPart(), b.URL.PathPart())
	})
	return w.out, nil
}

type walker struct {
	s    Store
	base *uri.URL
	pat  *glob.Pattern
	opts ExpandOptions
	seen map[string]bool
	out  []*Node
}

func (w *walker) log() *slog.Logger {
	if w.opts.Log != nil {
		return w.opts.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (w *walker) emit(n *Node) {
	if n == nil {
		return
	}
	// A pattern written with a trailing slash selects directories only, as it
	// does in a shell.
	if w.pat.TrailingSlash && !w.isDir(n) {
		return
	}
	key := n.URL.PathPart()
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	w.out = append(w.out, n)
}

// isDir reports whether a node can be descended into, honouring --dereference.
func (w *walker) isDir(n *Node) bool {
	if n.IsDir() {
		return true
	}
	return w.opts.Follow && n.IsSymlink() && n.Mode.IsDir()
}

// walk matches pattern segments from index i against the subtree at cur.
func (w *walker) walk(ctx context.Context, cur *uri.URL, i int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	segs := w.pat.Segs
	if i >= len(segs) {
		n, err := w.s.Stat(ctx, cur, w.opts.Follow)
		if err == nil {
			w.emit(n)
		} else if !IsNotExist(err) {
			w.log().Warn("cannot stat while expanding pattern",
				"path", cur.Display(), "error", err)
		}
		return nil
	}

	seg := segs[i]
	last := i == len(segs)-1

	if seg.Kind == glob.SegLiteral {
		child := cur.Join(seg.Text)
		if last {
			return w.walk(ctx, child, i+1)
		}
		n, err := w.s.Stat(ctx, child, w.opts.Follow)
		if err != nil {
			if !IsNotExist(err) {
				w.log().Warn("cannot stat while expanding pattern",
					"path", child.Display(), "error", err)
			}
			return nil
		}
		if !w.isDir(n) {
			return nil
		}
		return w.walk(ctx, child, i+1)
	}

	if seg.Kind == glob.SegDoubleStar {
		return w.walkDeep(ctx, cur, i)
	}

	// A single matched element: one listing, filtered by name.
	entries, err := w.s.ReadDir(ctx, cur)
	if err != nil {
		if !IsNotExist(err) {
			w.log().Warn("cannot list directory while expanding pattern",
				"path", cur.Display(), "error", err)
		}
		return nil
	}
	for _, e := range entries {
		if !seg.Match(e.Name()) {
			continue
		}
		if last {
			w.emit(e)
			continue
		}
		if w.isDir(e) {
			if err := w.walk(ctx, e.URL, i+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkDeep handles a "**" segment. Rather than descending one listing at a
// time, it takes a single recursive listing rooted at cur and matches the
// remaining pattern against each entry's relative path. For blob storage that
// turns an arbitrarily deep pattern into one prefixed listing instead of one
// request per prefix.
func (w *walker) walkDeep(ctx context.Context, cur *uri.URL, i int) error {
	// "**" can absorb zero elements, in which case cur itself is a candidate.
	if w.pat.MatchFrom(i, "") {
		if n, err := w.s.Stat(ctx, cur, w.opts.Follow); err == nil {
			w.emit(n)
		} else if !IsNotExist(err) {
			w.log().Warn("cannot stat while expanding pattern",
				"path", cur.Display(), "error", err)
		}
	}

	base := cur.PathPart()
	onError := func(u *uri.URL, err error) error {
		w.log().Warn("skipping unreadable path while expanding pattern",
			"path", u.Display(), "error", err)
		return nil
	}
	err := w.s.WalkAll(ctx, cur, onError, func(n *Node) error {
		rel, ok := relUnder(base, n.URL.PathPart())
		if !ok || rel == "" {
			return nil
		}
		if w.pat.MatchFrom(i, rel) {
			w.emit(n)
		}
		return nil
	})
	if err != nil && !IsNotExist(err) {
		w.log().Warn("recursive listing failed while expanding pattern",
			"path", cur.Display(), "error", err)
	}
	return ctx.Err()
}

// RelUnder returns child's path relative to base, comparing element by element
// so that ".", "//" and absolute paths all behave. The second result is false
// when child does not lie under base.
func RelUnder(base, child string) (string, bool) { return relUnder(base, child) }

func relUnder(base, child string) (string, bool) {
	b := glob.SplitPath(base)
	c := glob.SplitPath(child)
	if len(c) < len(b) {
		return "", false
	}
	for i := range b {
		if b[i] != c[i] {
			return "", false
		}
	}
	return strings.Join(c[len(b):], "/"), true
}
