// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/engine"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type blobStore interface {
	store.Store
	RemoveMarker(context.Context, *uri.URL) error
	RemoveBlob(context.Context, *store.Node) error
}

type backend struct {
	opt         cli.Options
	log         *slog.Logger
	local       store.Store
	remote      blobStore
	az          *azure.Store
	collections collections
}

func newBackend(opt *cli.Options, log *slog.Logger) (*backend, error) {
	c, err := loadCollections(opt.TUIConfig, opt.EndpointSuffix)
	if err != nil {
		return nil, err
	}
	az := azure.New(azure.Config{
		Auth: opt.Auth, Log: log, Interactive: true, TenantID: opt.TenantID,
		MaxRetries: int32(opt.Retries - 1), TryTimeout: opt.Timeout,
		CreateContainer: opt.CreateContainer, IncludeMetadata: true,
		UserAgent:    cli.Program + "/" + cli.VersionString(),
		PeakRequests: opt.PeakRequests(), BytesPerSecond: opt.BandwidthLimit,
	})
	return &backend{opt: *opt, log: log, local: local.New(log, false),
		remote: az, az: az, collections: c}, nil
}

func (b *backend) store(u *uri.URL) store.Store {
	if u.IsRemote() {
		return b.remote
	}
	return b.local
}

func (b *backend) resolve(raw string, relative location) (location, error) {
	if strings.HasPrefix(raw, "virtual://") || uri.IsRemoteArg(raw) || filepath.IsAbs(raw) || relative.url == nil {
		return b.collections.resolve(raw, b.opt.EndpointSuffix)
	}
	if raw == ".." {
		return b.collections.parent(relative, b.opt.EndpointSuffix), nil
	}
	if relative.url.IsRemote() {
		return location{url: relative.url.Join(raw)}, nil
	}
	return b.collections.resolve(filepath.Join(relative.url.Path, raw), b.opt.EndpointSuffix)
}

func (b *backend) list(ctx context.Context, l location) ([]entry, error) {
	if l.url == nil {
		return b.collections.list(l), nil
	}
	nodes, err := b.store(l.url).ReadDir(ctx, l.url)
	if err != nil {
		return nil, err
	}
	entries := make([]entry, 0, len(nodes))
	for _, n := range nodes {
		name := n.Name()
		if l.url.IsRemote() && l.url.Container != "" {
			prefix := l.url.Key
			if prefix != "" {
				prefix += "/"
			}
			name = strings.TrimPrefix(n.URL.Key, prefix)
		}
		child := location{url: n.URL, parent: &l}
		if l.address != "" {
			child.address = l.address + "/" + url.PathEscape(name)
		}
		if name == "" {
			// An empty blob-key component is a real directory. Its plain
			// Azure address is unambiguous; a trailing virtual-address slash
			// would instead be parsed as directory syntax. The parent link
			// still brings navigation back into the virtual hierarchy.
			name, child.address = "/", ""
		}
		entries = append(entries, entry{name: name, loc: child, node: n})
	}
	slices.SortStableFunc(entries, func(a, b entry) int {
		if a.directory() != b.directory() {
			if a.directory() {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	return entries, nil
}

func nodeURL(n *store.Node) *uri.URL {
	u := *n.URL
	// A blob and a prefix can share a name. Keep the selection's identity
	// when the engine or a fresh stat asks what was selected.
	u.TrailingSlash = n.IsDir() && u.IsRemote()
	return &u
}

func (b *backend) transfer(ctx context.Context, selected []entry, target location, move bool, p *progress.Reporter) error {
	if target.url == nil {
		return errors.New("open a folder inside the virtual container as the destination")
	}
	u := *target.url
	u.TrailingSlash = true
	dest, err := b.store(&u).Stat(ctx, &u, true)
	if err != nil || !dest.IsDir() {
		if err == nil {
			err = errors.New("destination is not a directory; use F7 to create one")
		}
		return err
	}
	if u.IsRemote() && u.Container == "" {
		return errors.New("open a destination container first")
	}
	o := b.opt
	o.TUI, o.Interactive, o.Verbose = false, false, false
	o.Recursive, o.CopyMetadata = true, true
	o.Glob, o.Deref = cli.GlobNever, cli.DerefNever
	o.Preserve = local.Preserve{Mode: true, Timestamps: true, Links: true}
	o.Sources = nil
	o.HasTargetDir = true
	o.Dest = operand(&u, true)
	for _, e := range selected {
		if e.node == nil {
			return errors.New("open a virtual container before copying its entries")
		}
		if move {
			if err := removable(e); err != nil {
				return err
			}
		}
		if e.node.IsDir() && inside(e.loc.url, &u) {
			return errors.New("destination is inside the source directory")
		}
		o.Sources = append(o.Sources, operand(e.loc.url, e.node.IsDir()))
	}
	if len(o.Sources) == 0 {
		return errors.New("nothing selected")
	}
	var before [][]*store.Node
	if move {
		before, err = b.inventory(ctx, selected)
		if err != nil {
			return err
		}
		for _, tree := range before {
			for _, n := range tree {
				if n.Kind == store.KindOther {
					return fmt.Errorf("cannot move special file %s", n.URL.Display())
				}
			}
		}
	}
	e, err := engine.New(engine.Config{Options: &o, Log: b.log, Progress: p, Azure: b.az})
	if err != nil {
		return err
	}
	failed, err := e.Run(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, skipped, _, _, _ := p.Totals()
	if failed > 0 || skipped > 0 {
		detail := ""
		if move {
			detail = "; move sources were retained"
		}
		return fmt.Errorf("%d failed, %d skipped%s", failed, skipped, detail)
	}
	if !move || o.DryRun {
		return nil
	}
	// Re-list before deleting anything: newly added, changed or disappeared
	// sources mean the copy cannot stand in for the source tree any more.
	after, err := b.inventory(ctx, selected)
	if err != nil {
		return fmt.Errorf("source retained: %w", err)
	}
	if !sameInventory(before, after) {
		return errors.New("source changed during the copy; all sources retained")
	}
	return b.remove(ctx, after)
}

func inside(parent, child *uri.URL) bool {
	if parent.IsRemote() != child.IsRemote() {
		return false
	}
	if parent.IsRemote() {
		return parent.SameAccount(child) && parent.Container == child.Container &&
			(parent.Key == "" || child.Key == parent.Key || strings.HasPrefix(child.Key, parent.Key+"/"))
	}
	p, err1 := filepath.EvalSymlinks(parent.Path)
	c, err2 := filepath.EvalSymlinks(child.Path)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(p, c)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func removable(e entry) error {
	if e.reference || e.node == nil || e.loc.url == nil {
		return errors.New("virtual references cannot be moved or deleted; open the folder first")
	}
	u := e.loc.url
	if u.IsRemote() {
		if u.Key == "" {
			return errors.New("accounts and Azure containers cannot be moved or deleted; select their contents")
		}
	} else if filepath.Dir(u.Path) == filepath.Clean(u.Path) {
		return errors.New("filesystem roots cannot be moved or deleted")
	}
	return nil
}

func (b *backend) inventory(ctx context.Context, selected []entry) ([][]*store.Node, error) {
	var trees [][]*store.Node
	for _, e := range selected {
		if err := removable(e); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := b.store(e.loc.url)
		n, err := s.Stat(ctx, nodeURL(e.node), false)
		if err != nil {
			return nil, err
		}
		if n.Kind != e.node.Kind {
			return nil, errors.New("selected entry changed; refresh before trying again")
		}
		tree := []*store.Node{n}
		seen := map[string]int{nodeKey(n): 0}
		if n.IsDir() {
			err = s.WalkAll(ctx, n.URL, func(_ *uri.URL, err error) error { return err }, func(child *store.Node) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				key := nodeKey(child)
				if index, ok := seen[key]; ok {
					// A prefix stat synthesizes the root; its own marker can
					// also arrive in the flat walk. Keep one entry, with the
					// marker's properties when it exists.
					if child.ETag != "" {
						tree[index] = child
					}
				} else {
					seen[key] = len(tree)
					tree = append(tree, child)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("cannot list %s in full: %w", n.URL.Display(), err)
			}
		}
		trees = append(trees, tree)
	}
	return trees, nil
}

func nodeKey(n *store.Node) string {
	return fmt.Sprintf("%s:%s:%s:%d", n.URL.Host, n.URL.Account, n.URL.PathPart(), n.Kind)
}

func sameNode(a, b *store.Node) bool {
	if nodeKey(a) != nodeKey(b) || a.Size != b.Size || !a.ModTime.Equal(b.ModTime) ||
		a.ETag != b.ETag || a.LinkTarget != b.LinkTarget || a.Mode != b.Mode {
		return false
	}
	return a.Info == nil || b.Info != nil && os.SameFile(a.Info, b.Info)
}

func sameInventory(a, b [][]*store.Node) bool {
	if len(a) != len(b) {
		return false
	}
	for i, tree := range a {
		if len(tree) != len(b[i]) {
			return false
		}
		byName := map[string]*store.Node{}
		for _, n := range tree {
			byName[nodeKey(n)] = n
		}
		for _, n := range b[i] {
			old := byName[nodeKey(n)]
			if old == nil || !sameNode(old, n) {
				return false
			}
		}
	}
	return true
}

func (b *backend) delete(ctx context.Context, selected []entry) error {
	// Nothing is removed on a partial listing, including an earlier selection
	// in the same batch. WalkAll never follows local symlinks here.
	trees, err := b.inventory(ctx, selected)
	if err != nil || b.opt.DryRun {
		return err
	}
	return b.remove(ctx, trees)
}

func (b *backend) remove(ctx context.Context, trees [][]*store.Node) error {
	for _, tree := range trees {
		if !tree[0].URL.IsRemote() {
			if err := removeLocal(ctx, tree); err != nil {
				return err
			}
			continue
		}
		// WalkAll may interleave key ranges. Depth, not arrival order, says
		// which marker can be removed after all its children.
		slices.SortStableFunc(tree, func(a, b *store.Node) int {
			return strings.Count(b.URL.Key, "/") - strings.Count(a.URL.Key, "/")
		})
		for _, n := range tree {
			if err := ctx.Err(); err != nil {
				return err
			}
			var err error
			if n.IsDir() {
				err = b.remote.RemoveMarker(ctx, n.URL)
				if store.IsNotExist(err) {
					err = nil
				}
			} else {
				err = b.remote.RemoveBlob(ctx, n)
			}
			if err != nil {
				return fmt.Errorf("cannot remove %s: %w", n.URL.Display(), err)
			}
		}
	}
	return nil
}

func removeLocal(ctx context.Context, tree []*store.Node) error {
	top := tree[0]
	parent, err := os.OpenRoot(filepath.Dir(top.URL.Path))
	if err != nil {
		return err
	}
	defer parent.Close()
	base := filepath.Base(top.URL.Path)
	info, err := parent.Lstat(base)
	if err != nil {
		return err
	}
	if top.Info == nil || !os.SameFile(top.Info, info) || top.Kind == store.KindDir && !info.IsDir() {
		return errors.New("source changed before removal; retained")
	}
	if top.IsDir() {
		root, err := parent.OpenRoot(base)
		if err != nil {
			return err
		}
		defer root.Close()
		opened, err := root.Stat(".")
		if err != nil {
			return err
		}
		if !os.SameFile(top.Info, opened) {
			return errors.New("source directory changed before removal; retained")
		}
		// An anchored root prevents an intervening directory symlink from
		// making a recursive removal reach outside the selected tree.
		slices.SortStableFunc(tree[1:], func(a, b *store.Node) int {
			return strings.Count(b.URL.Path, "/") - strings.Count(a.URL.Path, "/")
		})
		for _, n := range tree[1:] {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(top.URL.Path, n.URL.Path)
			if err != nil {
				return err
			}
			fi, err := root.Lstat(rel)
			if err != nil {
				return err
			}
			if !unchangedLocal(n, fi) {
				return errors.New("source changed before removal; retained")
			}
			if err := root.Remove(rel); err != nil {
				return err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err = parent.Lstat(base)
	if err != nil {
		return err
	}
	if !unchangedLocal(top, info) {
		return errors.New("source changed before removal; retained")
	}
	return parent.Remove(base)
}

func unchangedLocal(n *store.Node, info os.FileInfo) bool {
	if n.Info == nil || !os.SameFile(n.Info, info) || n.Info.Mode() != info.Mode() {
		return false
	}
	// Removing children changes directory times and sizes ourselves.
	return n.IsDir() || n.Size == info.Size() && n.ModTime.Equal(info.ModTime())
}

func (b *backend) mkdir(ctx context.Context, parent location, name string) error {
	if parent.url == nil {
		return errors.New("virtual containers are configured in tui.json; open a mount to create a folder")
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return errors.New("enter a single directory name")
	}
	u := parent.url.Join(name)
	if b.opt.DryRun {
		return nil
	}
	if err := b.store(u).MkdirAll(ctx, u, 0o777); err != nil {
		return err
	}
	if u.IsRemote() && u.Key != "" {
		return b.az.MkdirMarker(ctx, u)
	}
	return nil
}

func (b *backend) preview(ctx context.Context, e entry) (string, error) {
	if e.node == nil {
		return e.loc.display(), nil
	}
	n, err := b.store(e.loc.url).Stat(ctx, nodeURL(e.node), false)
	if err != nil {
		return "", err
	}
	if n.IsSymlink() {
		return "Symbolic link to " + n.LinkTarget, nil
	}
	if !n.IsRegular() {
		return fmt.Sprintf("%s\n%s\n%s", n.URL.Display(), n.Kind, n.Mode), nil
	}
	var r io.ReadCloser
	if n.URL.IsRemote() {
		r, err = b.az.OpenRead(ctx, n)
	} else {
		r, err = os.Open(n.URL.Path)
	}
	if err != nil {
		return "", err
	}
	defer r.Close()
	const limit = 64 << 10
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return "", err
	}
	suffix := ""
	if len(data) > limit {
		data = data[:limit]
		suffix = "\n[Preview limited to 64 KiB]"
	}
	if strings.ContainsRune(string(data), 0) {
		return "Binary file (text preview unavailable)", nil
	}
	return strings.ToValidUTF8(string(data), "�") + suffix, nil
}
