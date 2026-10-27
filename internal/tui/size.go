// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"

	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type directorySize struct {
	bytes, files int64
}

// sizeTree keeps one total per directory the walk passed through, keyed by
// the names the listing gives entries, so entering a sized directory needs
// no second walk. It costs one node per directory, never one per file.
type sizeTree struct {
	directorySize
	children map[string]*sizeTree
	gen      uint64
	// stale says a mutation below this directory outdated its own total.
	// Its untouched children are still exact, so the node stays to hold them.
	stale bool
}

func (t *sizeTree) child(name string) *sizeTree {
	name = sizeName(name)
	c := t.children[name]
	if c == nil {
		if t.children == nil {
			t.children = map[string]*sizeTree{}
		}
		c = &sizeTree{}
		t.children[name] = c
	}
	return c
}

// sizeChange names what a mutation may have altered: the location at, or the
// entry within it named by tail.
type sizeChange struct {
	at   *uri.URL
	tail []string
}

// forgetSizes drops the totals a mutation can have changed: the tree of each
// changed location and the total of every directory above one. Siblings keep
// theirs, so a sorted pane does not lose its order to an unrelated move.
func (a *app) forgetSizes(changes []sizeChange) {
	for i := range a.panes {
		for l := &a.panes[i].loc; l != nil; l = l.parent {
			if l.sizes == nil {
				continue
			}
			for _, c := range changes {
				l.sizes.forget(l.url, c)
			}
		}
	}
}

func (t *sizeTree) forget(root *uri.URL, c sizeChange) {
	paths := sizeRelations(root, c.at)
	if len(paths) == 0 {
		if len(sizeRelations(c.at, root)) > 0 {
			// The whole tree lies inside what changed.
			t.stale, t.children = true, nil
		}
		return
	}
	for _, path := range paths {
		path = append(path, c.tail...)
		if len(path) == 0 {
			t.stale, t.children = true, nil
			continue
		}
		n := t
		for _, name := range path[:len(path)-1] {
			n.stale = true
			if n = n.children[sizeName(name)]; n == nil {
				break
			}
		}
		if n != nil {
			n.stale = true
			delete(n.children, sizeName(path[len(path)-1]))
		}
	}
}

func (d *dialog) changes() []sizeChange {
	if d.action == "mkdir" {
		return []sizeChange{{at: d.target.url, tail: []string{d.text}}}
	}
	var changes []sizeChange
	for _, e := range d.source {
		if e.node == nil {
			continue
		}
		if d.action != "Copy" {
			changes = append(changes, sizeChange{at: e.node.URL})
		}
		if d.action != "Delete" {
			changes = append(changes, sizeChange{at: d.target.url, tail: []string{sizeBase(e.node.URL)}})
		}
	}
	return changes
}

// sizeRelations gives the names leading from root to u, once for each way
// root's URL can be read. A container root and the empty key component
// beneath it share a URL; forgetting under both readings costs at most a
// total that was still right.
func sizeRelations(root, u *uri.URL) [][]string {
	if root.IsRemote() != u.IsRemote() {
		return nil
	}
	if !root.IsRemote() {
		if path, below := sizePath(root, false, u); below {
			return [][]string{path}
		}
		return nil
	}
	if root.Host != u.Host || root.Account != u.Account {
		return nil
	}
	if root.Container == u.Container && root.Key == u.Key {
		return [][]string{nil}
	}
	var paths [][]string
	for _, prefixed := range []bool{false, true} {
		if prefixed && root.Container == "" || !prefixed && root.Key != "" {
			continue
		}
		if path, below := sizePath(root, prefixed, u); below {
			paths = append(paths, path)
		}
	}
	return paths
}

// sizeBase is the name a copy of u takes inside its destination directory.
func sizeBase(u *uri.URL) string {
	switch {
	case !u.IsRemote():
		return filepath.Base(u.Path)
	case u.Key == "":
		return u.Container
	default:
		return u.Key[strings.LastIndexByte(u.Key, '/')+1:]
	}
}

func sizeName(name string) string {
	if name == "" {
		return "/" // How the listing names a significant empty blob-key component.
	}
	return name
}

func (a *app) calculateSizes(all bool) {
	p := &a.panes[a.active]
	var indices []int
	hasMarks := false
	for _, marked := range p.selected {
		hasMarks = hasMarks || marked
	}
	for i, e := range p.entries {
		if i == 0 || !e.node.IsDir() {
			continue
		}
		if all || p.selected[i] || !hasMarks && i == p.cursor {
			indices = append(indices, i)
		}
	}
	if len(indices) == 0 {
		a.status = "Select a directory first (symbolic links are not followed)"
		return
	}
	selected := make([]entry, len(indices))
	for i, row := range indices {
		p.entries[row].dirSize = nil
		p.entries[row].sizePending = true
		selected[i] = p.entries[row]
	}
	base := p.loc
	if p.loc.sizes == nil {
		p.loc.sizes = &sizeTree{gen: a.sizeGen}
	}
	if p.loc.sizes.children == nil {
		p.loc.sizes.children = map[string]*sizeTree{}
	}
	a.start("Calculating directory totals", func(ctx context.Context, progress *progress.Reporter) (any, error) {
		return a.backend.directorySizes(ctx, base, selected, all, progress)
	}, func(r jobResult) {
		for _, row := range indices {
			p.entries[row].sizePending = false
		}
		if r.err == nil {
			trees := r.value.([]*sizeTree)
			var total directorySize
			for i, row := range indices {
				e := &p.entries[row]
				trees[i].gen = p.loc.sizes.gen
				p.loc.sizes.children[e.name] = trees[i]
				e.dirSize, e.loc.sizes = &trees[i].directorySize, trees[i]
				total.bytes += trees[i].bytes
				total.files += trees[i].files
			}
			a.status = fmt.Sprintf("%d directories: %s in %s files (logical sizes)", len(trees), humanize.Bytes(total.bytes), humanize.Count(total.files))
		}
		p.sortEntries()
	})
	a.busy.sizing = true
}

// directorySizes adds metadata lengths, never reads file bodies, and retains
// one total per directory below each selection. A failed or cancelled walk
// supplies no totals: a partial listing must not masquerade as an empty or
// small tree.
func (b *backend) directorySizes(ctx context.Context, base location, selected []entry, all bool, p *progress.Reporter) ([]*sizeTree, error) {
	trees := make([]*sizeTree, len(selected))
	for i := range trees {
		trees[i] = &sizeTree{}
	}
	p.SetScanning(true)
	var total int64
	// path names n's location below selection i, and is empty for the
	// selection itself. Every total along it shares the file's bytes.
	add := func(i int, path []string, n *store.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		t := trees[i]
		if n.IsDir() {
			for _, name := range path {
				t = t.child(name)
			}
			return nil
		}
		if !n.IsRegular() || len(path) == 0 {
			return nil
		}
		if n.Size < 0 || n.Size > math.MaxInt64-total {
			return errors.New("directory size exceeds the supported range")
		}
		for j := 0; ; j++ {
			t.bytes += n.Size
			t.files++
			if j == len(path)-1 {
				break
			}
			t = t.child(path[j])
		}
		total += n.Size
		p.Plan(1, n.Size)
		return nil
	}
	walk := func(u *uri.URL, fn func(*store.Node) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := b.store(u).WalkAll(ctx, u, func(_ *uri.URL, err error) error { return err }, fn)
		if err != nil {
			return fmt.Errorf("cannot calculate sizes under %s: %w", u.Display(), err)
		}
		return ctx.Err()
	}
	if all {
		// The whole pane is one flat Azure walk, not one listing per prefix.
		// At account level WalkAll reuses its bounded container look-ahead.
		children := make(map[string]int, len(selected))
		for i, e := range selected {
			if name, ok := sizeChildName(base.url, e.loc.url, true); ok {
				children[name] = i
			}
		}
		err := walk(base.url, func(n *store.Node) error {
			if name, below := sizeChildName(base.url, n.URL, false); below {
				if i, ok := children[name]; ok {
					path, _ := sizePath(base.url, base.url.Key != "", n.URL)
					return add(i, path[1:], n)
				}
			}
			return ctx.Err()
		})
		if err != nil {
			return nil, err
		}
		return trees, nil
	}
	for i, e := range selected {
		u := nodeURL(e.node)
		if !u.IsRemote() {
			// A stale selection may now be a symlink; never follow it as a root.
			n, err := b.local.Stat(ctx, u, false)
			if err != nil {
				return nil, err
			}
			if !n.IsDir() {
				return nil, errors.New("selected directory changed; refresh before trying again")
			}
		}
		if err := walk(u, func(n *store.Node) error {
			// The empty component at a container root has the same URL key
			// as the container. Only names starting with "/" belong to it.
			path, below := sizePath(u, base.url.Container != "", n.URL)
			if !below {
				return ctx.Err()
			}
			return add(i, path, n)
		}); err != nil {
			return nil, err
		}
	}
	return trees, nil
}

// sizePath splits the location of child below root into the names the
// listing gives each level. prefixed says root is a prefix inside a
// container, whose children follow a "/" even when its own key is empty.
func sizePath(root *uri.URL, prefixed bool, child *uri.URL) ([]string, bool) {
	var rel string
	switch {
	case !root.IsRemote():
		path, err := filepath.Rel(root.Path, child.Path)
		if err != nil || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return nil, false
		}
		if path == "." {
			return nil, true
		}
		return strings.Split(filepath.ToSlash(path), "/"), true
	case root.Container == "":
		if child.Container == "" {
			return nil, false
		}
		if child.Key == "" {
			return []string{child.Container}, true
		}
		return append([]string{child.Container}, strings.Split(child.Key, "/")...), true
	case child.Container != root.Container:
		return nil, false
	case prefixed:
		var ok bool
		if rel, ok = strings.CutPrefix(child.Key, root.Key+"/"); !ok {
			return nil, false
		}
	default:
		if child.Key == "" {
			return nil, true
		}
		rel = child.Key
	}
	return strings.Split(rel, "/"), true
}

// Preserve significant empty components and dot components in blob names.
// A regular blob named exactly like a directory is not inside that directory.
func sizeChildName(base, child *uri.URL, directory bool) (string, bool) {
	if base.IsRemote() && base.Container == "" {
		return child.Container, child.Container != ""
	}
	var rel string
	if base.IsRemote() {
		prefix := base.Key
		if prefix != "" {
			prefix += "/"
		}
		if !strings.HasPrefix(child.Key, prefix) {
			return "", false
		}
		rel = strings.TrimPrefix(child.Key, prefix)
	} else {
		path, err := filepath.Rel(base.Path, child.Path)
		if err != nil {
			return "", false
		}
		rel = filepath.ToSlash(path)
	}
	if directory {
		return rel, true
	}
	name, _, below := strings.Cut(rel, "/")
	return name, below
}
