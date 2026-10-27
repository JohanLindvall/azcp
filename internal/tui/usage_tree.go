// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type usageTotals struct {
	logical, disk, files, dirs int64
	newest                     time.Time
	incomplete, diskUnknown    bool
	excluded                   int64
}

// The cache retains names and totals, not a full store.Node, URL, credentials,
// metadata map and platform stat structure for every file in a large tree.
type usageNode struct {
	name      string
	parent    *usageNode
	children  []*usageNode
	kind      store.Kind
	mode      fs.FileMode
	modified  time.Time
	length    int64
	allocated int64
	diskKnown bool
	link      *usageLink
	problem   string
	excluded  bool
	marker    bool
	total     usageTotals
}

type usageLink struct {
	count uint64
	seen  map[*usageNode]bool
}

type usageTree struct {
	base       location
	root       *usageNode
	prefix     string
	oneFS      bool
	scanned    time.Time
	issues     []string
	issueCount int
}

func (n *usageNode) relative() string {
	var parts []string
	for p := n; p.parent != nil; p = p.parent {
		parts = append(parts, p.name)
	}
	slices.Reverse(parts)
	return strings.Join(parts, "/")
}

func (t *usageTree) location(n *usageNode) location {
	if n == t.root {
		return t.base
	}
	u := t.base.url
	if u.IsRemote() {
		path := n.relative()
		if u.Container != "" {
			path = u.Container + "/" + t.prefix + path
		}
		u = u.WithPathPart(path)
	} else {
		u = u.Join(n.relative())
	}
	parent := t.location(n.parent)
	return location{url: u, parent: &parent}
}

func (t *usageTree) entry(n *usageNode) entry {
	l := t.location(n)
	name := n.name
	if name == "" {
		name = "/"
	}
	return entry{name: name, loc: l, usage: n, node: &store.Node{
		URL: l.url, Kind: n.kind, Mode: n.mode, Size: n.length, ModTime: n.modified,
	}}
}

func (t *usageTree) problem(n *usageNode, err error) {
	if n.problem == "" {
		n.problem = err.Error()
	}
	t.issueCount++
	if len(t.issues) < 100 {
		t.issues = append(t.issues, fmt.Sprintf("cannot scan %s: %v", t.location(n).display(), err))
	}
}

func (t *usageTree) find(relative string) *usageNode {
	if relative == "" {
		return t.root
	}
	n := t.root
	for _, name := range strings.Split(strings.TrimSuffix(relative, "/"), "/") {
		var child *usageNode
		for _, c := range n.children {
			if c.name == name && c.kind == store.KindDir {
				child = c
				break
			}
		}
		if child == nil {
			return n
		}
		n = child
	}
	return n
}

// Each directory counts an inode once, even when aliases occur in different
// descendants. Sibling directories can share bytes; their totals need not add
// up to their parent's. File counts still count names, as the browser does.
func (t *usageTree) recount(ctx context.Context) error {
	var nodes []*usageNode
	stack := []*usageNode{t.root}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n.total = usageTotals{}
		if n.link != nil {
			n.link.seen = make(map[*usageNode]bool)
		}
		nodes = append(nodes, n)
		stack = append(stack, n.children...)
	}
	add := func(to *int64, value int64) bool {
		if value < 0 || value > math.MaxInt64-*to {
			*to = math.MaxInt64
			return false
		}
		*to += value
		return true
	}
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		for p := n; p != nil; p = p.parent {
			if n.excluded {
				p.total.excluded++
				continue
			}
			p.total.incomplete = p.total.incomplete || n.problem != ""
			p.total.diskUnknown = p.total.diskUnknown || !n.diskKnown
			if n.kind == store.KindFile {
				p.total.files++
				if n.modified.After(p.total.newest) {
					p.total.newest = n.modified
				}
			}
			if n.kind == store.KindDir && n != p {
				p.total.dirs++
			}
			if n.link != nil && p != n {
				if n.link.seen[p] {
					continue
				}
				n.link.seen[p] = true
			}
			logicalOK, diskOK := add(&p.total.logical, n.length), add(&p.total.disk, n.allocated)
			if !logicalOK || !diskOK {
				p.total.incomplete = true
				p.problem = "total exceeds the supported range"
			}
		}
	}
	for _, n := range nodes {
		if n.link != nil {
			n.link.seen = nil
		}
	}
	return nil
}

// Deletion changes only the chosen subtrees. Build the replacement cache in
// the worker, leaving the displayed snapshot immutable until it is ready.
// Recounting shared inodes avoids subtracting bytes still reachable elsewhere.
func (t *usageTree) without(ctx context.Context, selected []entry) (*usageTree, error) {
	remove := make(map[*usageNode]bool, len(selected))
	removedLinks := make(map[*usageLink]uint64)
	for _, e := range selected {
		if e.usage == nil || e.usage == t.root {
			return nil, errors.New("invalid usage selection")
		}
		remove[e.usage] = true
		stack := []*usageNode{e.usage}
		for len(stack) > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if n.link != nil {
				removedLinks[n.link]++
			}
			stack = append(stack, n.children...)
		}
	}
	out := *t
	links := make(map[*usageLink]*usageLink)
	var clone func(*usageNode, *usageNode) (*usageNode, error)
	clone = func(n, parent *usageNode) (*usageNode, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		x := *n
		x.parent, x.children = parent, nil
		if n.link != nil {
			if links[n.link] == nil {
				links[n.link] = &usageLink{count: n.link.count - min(n.link.count, removedLinks[n.link])}
			}
			x.link = links[n.link]
		}
		if x.problem == "total exceeds the supported range" {
			x.problem = ""
		}
		for _, child := range n.children {
			if remove[child] {
				continue
			}
			c, err := clone(child, &x)
			if err != nil {
				return nil, err
			}
			if t.base.url.IsRemote() && c.kind == store.KindDir && !c.marker && len(c.children) == 0 && c.problem == "" {
				continue
			}
			x.children = append(x.children, c)
		}
		return &x, nil
	}
	var err error
	out.root, err = clone(t.root, nil)
	if err != nil {
		return nil, err
	}
	if err := out.recount(ctx); err != nil {
		return nil, err
	}
	return &out, nil
}

func (b *backend) usageAfterDelete(ctx context.Context, old *usageTree, selected []entry) (*usageTree, error) {
	tree, err := old.without(ctx, selected)
	if err != nil || tree.base.url.IsRemote() {
		return tree, err
	}
	// All selections come from one pane directory. Its own directory allocation
	// and timestamp may change when entries are removed, even if its surviving
	// children's sizes do not. One stat refreshes that metadata without a scan.
	parent := selected[0].usage.parent
	key := ""
	if parent != old.root {
		key = parent.relative() + "/"
	}
	current := tree.find(key)
	u := tree.location(current).url
	n, statErr := b.local.Stat(ctx, u, false)
	if statErr != nil {
		return nil, fmt.Errorf("deletion completed; cannot refresh parent %s: %w", u.Display(), statErr)
	} else if !n.IsDir() {
		return nil, errors.New("deletion completed; parent directory changed, so cached entries were discarded")
	} else {
		disk, err := b.usageOf(u.Path, n.Info)
		current.length, current.modified = max(0, n.Size), n.ModTime
		current.allocated, current.diskKnown = max(0, disk.Allocated), disk.AllocationKnown
		if err != nil {
			tree.problem(current, err)
		}
	}
	if err := tree.recount(ctx); err != nil {
		return nil, err
	}
	return tree, nil
}

func (b *backend) scanUsage(ctx context.Context, base location, oneFS bool, p *progress.Reporter) (*usageTree, error) {
	u := *base.url
	u.TrailingSlash = u.IsRemote()
	root, err := b.store(&u).Stat(ctx, &u, false)
	if err != nil {
		return nil, err
	}
	if !root.IsDir() {
		return nil, errors.New("usage scans require a directory; symbolic links are not followed")
	}
	t := &usageTree{base: base, oneFS: oneFS}
	links := make(map[local.FileID]*usageLink)
	var progressBytes int64
	p.SetScanning(true)
	makeNode := func(parent *usageNode, name string, n *store.Node) (*usageNode, local.Usage) {
		x := &usageNode{name: name, parent: parent, kind: n.Kind, mode: n.Mode,
			modified: n.ModTime, length: max(0, n.Size), marker: !n.URL.IsRemote() || n.ETag != "" || n.URL.Key == ""}
		if parent != nil {
			parent.children = append(parent.children, x)
		} else {
			t.root = x
		}
		var disk local.Usage
		if n.URL.IsRemote() {
			x.allocated, x.diskKnown = x.length, true
		} else {
			var err error
			disk, err = b.usageOf(n.URL.Path, n.Info)
			x.allocated, x.diskKnown = max(0, disk.Allocated), disk.AllocationKnown
			if err != nil {
				t.problem(x, err)
			}
			if n.IsRegular() && disk.IdentityKnown && disk.Links > 1 {
				link := links[disk.ID]
				if link == nil {
					link = &usageLink{count: disk.Links}
					links[disk.ID] = link
				}
				x.link = link
			}
		}
		if n.IsRegular() {
			bytes := x.length
			if bytes > math.MaxInt64-progressBytes {
				bytes = math.MaxInt64 - progressBytes
				t.problem(x, errors.New("total exceeds the supported range"))
			}
			progressBytes += bytes
			p.Plan(1, bytes)
		}
		if n.Size < 0 || disk.Allocated < 0 {
			t.problem(x, errors.New("invalid negative size reported by storage"))
		}
		return x, disk
	}
	r, rootDisk := makeNode(nil, root.Name(), root)
	if !u.IsRemote() {
		if oneFS && !rootDisk.IdentityKnown {
			return nil, errors.New("cannot determine the root filesystem; unable to enforce one-filesystem scanning")
		}
		visited := make(map[local.FileID]bool)
		if rootDisk.IdentityKnown {
			visited[rootDisk.ID] = true
		}
		var walk func(*usageNode, *uri.URL, int) error
		walk = func(parent *usageNode, at *uri.URL, depth int) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if depth > 256 {
				t.problem(parent, errors.New("directory nesting exceeds 256 levels"))
				return nil
			}
			visit := func(n *store.Node) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				x, disk := makeNode(parent, n.Name(), n)
				if !n.IsDir() {
					return nil
				}
				if oneFS && !disk.IdentityKnown {
					x.excluded, x.problem = true, "cannot identify this directory's filesystem"
					return nil
				}
				if oneFS && rootDisk.IdentityKnown && disk.IdentityKnown && disk.ID.Dev != rootDisk.ID.Dev {
					x.excluded, x.problem = true, "different filesystem (x toggles filesystem boundaries)"
					return nil
				}
				if disk.IdentityKnown {
					if visited[disk.ID] {
						x.excluded, x.problem = true, "directory alias already scanned"
						return nil
					}
					visited[disk.ID] = true
				}
				return walk(x, n.URL, depth+1)
			}
			var err error
			if stream, ok := b.local.(interface {
				ReadDirEach(context.Context, *uri.URL, func(*uri.URL, error) error, func(*store.Node) error) error
			}); ok {
				err = stream.ReadDirEach(ctx, at, func(_ *uri.URL, err error) error {
					t.problem(parent, err)
					return ctx.Err()
				}, visit)
			} else {
				var nodes []*store.Node
				nodes, err = b.local.ReadDir(ctx, at)
				if err == nil {
					for _, n := range nodes {
						if err = visit(n); err != nil {
							break
						}
					}
				}
			}
			if err != nil && ctx.Err() == nil {
				t.problem(parent, err)
			}
			return ctx.Err()
		}
		err = walk(r, &u, 0)
	} else {
		// Directory keys include their delimiter. This distinguishes the root
		// from its empty-name child, and a prefix from a same-name regular blob.
		if u.Container != "" && u.Key != "" {
			t.prefix = u.Key + "/"
		} else if u.Container != "" && base.parent != nil && base.parent.url.Container == u.Container {
			t.prefix = "/"
		}
		dirs := map[string]*usageNode{"": r}
		var directory func(string) *usageNode
		directory = func(key string) *usageNode {
			if d := dirs[key]; d != nil {
				return d
			}
			path := strings.TrimSuffix(key, "/")
			parentKey, name := "", path
			if i := strings.LastIndexByte(path, '/'); i >= 0 {
				parentKey, name = path[:i+1], path[i+1:]
			}
			parent := directory(parentKey)
			x := &usageNode{name: name, parent: parent, kind: store.KindDir, diskKnown: true}
			parent.children = append(parent.children, x)
			dirs[key] = x
			return x
		}
		err = b.remote.WalkAll(ctx, &u, func(at *uri.URL, err error) error {
			t.problem(r, fmt.Errorf("%s: %w", at.Display(), err))
			return ctx.Err()
		}, func(n *store.Node) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := n.URL.PathPart()
			prefix := ""
			if u.Container != "" {
				path, prefix = n.URL.Key, t.prefix
			}
			if n.IsDir() {
				path += "/"
			}
			if !strings.HasPrefix(path, prefix) {
				return nil
			}
			rel := strings.TrimPrefix(path, prefix)
			if n.IsDir() {
				d := directory(rel)
				d.mode, d.modified = n.Mode, n.ModTime
				d.marker = d.marker || n.ETag != "" || n.URL.Container == "" || n.URL.Key == "" && u.Container == ""
				return nil
			}
			parentKey, name := "", rel
			if i := strings.LastIndexByte(rel, '/'); i >= 0 {
				parentKey, name = rel[:i+1], rel[i+1:]
			}
			makeNode(directory(parentKey), name, n)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			t.problem(r, err)
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	if err := t.recount(ctx); err != nil {
		return nil, err
	}
	t.scanned = time.Now()
	return t, nil
}
