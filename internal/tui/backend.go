// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"golang.org/x/term"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/engine"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/retryx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type blobStore interface {
	store.Store
	ReadDirEntries(context.Context, *uri.URL) ([]*store.Node, error)
	RemoveMarker(context.Context, *uri.URL) error
	RemoveBlob(context.Context, *store.Node) error
	RemoveContainer(context.Context, *uri.URL) error
}

type backend struct {
	opt     cli.Options
	log     *slog.Logger
	local   store.Store
	remote  blobStore
	az      *azure.Store
	usageOf func(string, fs.FileInfo) (local.Usage, error)
}

func newBackend(opt *cli.Options, log *slog.Logger) *backend {
	az := azure.New(azure.Config{
		Auth: opt.Auth, Log: log, Interactive: term.IsTerminal(int(os.Stderr.Fd())), TenantID: opt.TenantID,
		MaxRetries: int32(opt.Retries - 1), TryTimeout: opt.Timeout,
		CreateContainer: opt.CreateContainer, IncludeMetadata: true,
		UserAgent:    cli.Program + "/" + cli.VersionString(),
		PeakRequests: opt.PeakRequests(), BytesPerSecond: opt.BandwidthLimit,
	})
	return &backend{opt: *opt, log: log, local: local.New(log, false),
		remote: az, az: az, usageOf: local.UsageOf}
}

func (b *backend) store(u *uri.URL) store.Store {
	if u.IsRemote() {
		return b.remote
	}
	return b.local
}

func (b *backend) resolve(raw string, relative location) (location, error) {
	if raw == "." {
		return relative, nil
	}
	if uri.IsRemoteArg(raw) || filepath.IsAbs(raw) || relative.url == nil {
		return parseLocation(raw, b.opt.EndpointSuffix)
	}
	if raw == ".." {
		return parentLocation(relative), nil
	}
	if relative.url.IsRemote() {
		return location{url: relative.url.Join(raw)}, nil
	}
	return parseLocation(filepath.Join(relative.url.Path, raw), b.opt.EndpointSuffix)
}

func (b *backend) list(ctx context.Context, l location) ([]entry, error) {
	var nodes []*store.Node
	var err error
	if l.url.IsRemote() {
		nodes, err = b.remote.ReadDirEntries(ctx, l.url)
	} else {
		nodes, err = b.local.ReadDir(ctx, l.url)
	}
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
		if name == "" {
			// Repeated slashes are significant blob-key components. Display
			// the empty component without cleaning away its exact URL.
			name = "/"
		}
		entries = append(entries, entry{name: name, loc: child, node: n})
	}
	slices.SortStableFunc(entries, (paneSort{}).compare)
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
			return errors.New("select a file or directory first")
		}
		if sharesContainerURL(e) {
			return errSharedURL
		}
		if move {
			if err := movable(e); err != nil {
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
	o.Jobs = o.JobsFor(u.IsRemote() || slices.ContainsFunc(selected, func(e entry) bool { return e.loc.url.IsRemote() }))
	var before []inventoried
	if move {
		before, err = b.inventory(ctx, selected)
		if err != nil {
			return err
		}
		for _, tree := range before {
			for _, n := range tree.nodes {
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
	if e.node == nil || e.loc.url == nil {
		return errors.New("select a file or directory first")
	}
	u := e.loc.url
	if u.IsRemote() {
		if u.Container == "" {
			return errors.New("an Azure account cannot be moved or deleted; select its containers")
		}
		if sharesContainerURL(e) {
			return errSharedURL
		}
	} else if filepath.Dir(u.Path) == filepath.Clean(u.Path) {
		return errors.New("filesystem roots cannot be moved or deleted")
	}
	return nil
}

// errSharedURL refuses the entry a container root lists for the blob names
// that begin with "/". No URL addresses those names apart from the container
// itself, so copying, moving or deleting the entry would take every blob in it.
var errSharedURL = errors.New(`"/" at a container root has the container's own URL, so it cannot be copied, moved or deleted as one directory`)

// sharesContainerURL reports the entry errSharedURL refuses: listed inside a
// container, yet addressed as the container.
func sharesContainerURL(e entry) bool {
	return e.loc.url != nil && isContainer(e.loc.url) && e.loc.parent != nil && e.loc.parent.url.Container != ""
}

// movable is removable, less containers: a move would recreate one
// container's contents inside another, and leave the source container behind
// or delete it, neither of which is what moving a container should mean.
func movable(e entry) error {
	if err := removable(e); err != nil {
		return err
	}
	if isContainer(e.loc.url) {
		return errors.New("Azure containers cannot be moved; open one and select its contents")
	}
	return nil
}

func isContainer(u *uri.URL) bool { return u.IsRemote() && u.Container != "" && u.Key == "" }

// inventoried is one selection listed in full: the selected node first, then
// everything below it. A local selection also keeps the directory it was found
// in, because removing it follows the path to that directory again.
type inventoried struct {
	nodes  []*store.Node
	parent fs.FileInfo
}

// errSelectionChanged says a selection is no longer the kind of thing that
// was selected.
var errSelectionChanged = errors.New("selected entry changed; refresh before trying again")

// errChanged says a removal found something other than what was listed.
var errChanged = errors.New("changed since it was listed; left in place")

func (b *backend) inventory(ctx context.Context, selected []entry) ([]inventoried, error) {
	var trees []inventoried
	for _, e := range selected {
		tree, err := b.inventoryOne(ctx, e)
		if err != nil {
			return nil, err
		}
		trees = append(trees, tree)
	}
	return trees, nil
}

func (b *backend) inventoryOne(ctx context.Context, e entry) (inventoried, error) {
	if err := removable(e); err != nil {
		return inventoried{}, err
	}
	if err := ctx.Err(); err != nil {
		return inventoried{}, err
	}
	s := b.store(e.loc.url)
	n, err := s.Stat(ctx, nodeURL(e.node), false)
	if err != nil {
		return inventoried{}, err
	}
	if n.Kind != e.node.Kind {
		return inventoried{}, errSelectionChanged
	}
	tree := inventoried{nodes: []*store.Node{n}}
	if !n.URL.IsRemote() {
		if tree.parent, err = os.Stat(filepath.Dir(n.URL.Path)); err != nil {
			return inventoried{}, err
		}
	}
	seen := map[string]int{nodeKey(n): 0}
	// A container is deleted in one request, so listing its blobs would
	// only delay that; the stat has already shown it is there to delete.
	if n.IsDir() && !isContainer(n.URL) {
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
					tree.nodes[index] = child
				}
			} else {
				seen[key] = len(tree.nodes)
				tree.nodes = append(tree.nodes, child)
			}
			return nil
		})
		if err != nil {
			return inventoried{}, &listError{url: n.URL, err: err}
		}
	}
	return tree, nil
}

// listError is a selection that could not be listed in full, which leaves
// nothing in it safe to remove.
type listError struct {
	url *uri.URL
	err error
}

func (e *listError) Error() string {
	why := reason(e.err)
	var pe *fs.PathError
	if errors.As(e.err, &pe) {
		// The walk names the directory it could not read; that is the news.
		why = engine.Quote(pe.Path) + ": " + why
	}
	return fmt.Sprintf("cannot list %s in full: %s", engine.Quote(e.url.Display()), why)
}

func (e *listError) Unwrap() error { return e.err }

// removeError names what a removal stopped at, which may lie deep inside the
// selection it came from.
type removeError struct {
	node *store.Node
	err  error
}

func (e *removeError) Error() string {
	return fmt.Sprintf("cannot remove %s: %s", engine.Quote(e.node.URL.Display()), reason(e.err))
}

func (e *removeError) Unwrap() error { return e.err }

// reason words a failure the way rm does: the system's message for a
// filesystem error and a service failure's status and code, never the SDK's
// dump of the whole exchange.
func reason(err error) string {
	var pe *fs.PathError
	var re *azcore.ResponseError
	switch {
	case store.IsNotExist(err):
		return "No such file or directory"
	case errors.As(err, &pe):
		return engine.Sentence(pe.Err)
	case errors.As(err, &re) && re.StatusCode == http.StatusPreconditionFailed:
		// The delete named the revision that was listed, and it is gone.
		return errChanged.Error()
	}
	return retryx.Describe(err)
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

func sameInventory(a, b []inventoried) bool {
	if len(a) != len(b) {
		return false
	}
	for i, tree := range a {
		if len(tree.nodes) != len(b[i].nodes) {
			return false
		}
		if tree.parent != nil && (b[i].parent == nil || !os.SameFile(tree.parent, b[i].parent)) {
			return false
		}
		byName := map[string]*store.Node{}
		for _, n := range tree.nodes {
			byName[nodeKey(n)] = n
		}
		for _, n := range b[i].nodes {
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

func (b *backend) remove(ctx context.Context, trees []inventoried) error {
	for _, tree := range trees {
		top := tree.nodes[0]
		if !top.URL.IsRemote() {
			if err := removeLocal(ctx, tree); err != nil {
				return err
			}
			continue
		}
		if isContainer(top.URL) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := b.remote.RemoveContainer(ctx, top.URL); err != nil {
				return &removeError{node: top, err: err}
			}
			continue
		}
		// WalkAll may interleave key ranges. Depth, not arrival order, says
		// which marker can be removed after all its children.
		nodes := slices.Clone(tree.nodes)
		slices.SortStableFunc(nodes, func(a, b *store.Node) int {
			return strings.Count(b.URL.Key, "/") - strings.Count(a.URL.Key, "/")
		})
		for _, n := range nodes {
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
				return &removeError{node: n, err: err}
			}
		}
	}
	return nil
}

func removeLocal(ctx context.Context, tree inventoried) error {
	top := tree.nodes[0]
	fail := func(n *store.Node, err error) error { return &removeError{node: n, err: err} }
	parent, err := os.OpenRoot(filepath.Dir(top.URL.Path))
	if err != nil {
		return fail(top, err)
	}
	defer parent.Close()
	// The path to the selection is followed again here. A directory swapped
	// in on the way could hold a hard link to a selected file, which would
	// pass for it, so the directory itself has to be the one listed.
	if tree.parent != nil {
		here, err := parent.Stat(".")
		if err != nil {
			return fail(top, err)
		}
		if !os.SameFile(tree.parent, here) {
			return fail(top, errChanged)
		}
	}
	base := filepath.Base(top.URL.Path)
	info, err := parent.Lstat(base)
	if err != nil {
		return fail(top, err)
	}
	if top.Info == nil || !os.SameFile(top.Info, info) || top.Kind == store.KindDir && !info.IsDir() {
		return fail(top, errChanged)
	}
	if top.IsDir() {
		root, err := parent.OpenRoot(base)
		if err != nil {
			return fail(top, err)
		}
		defer root.Close()
		opened, err := root.Stat(".")
		if err != nil {
			return fail(top, err)
		}
		if !os.SameFile(top.Info, opened) {
			return fail(top, errChanged)
		}
		// An anchored root prevents an intervening directory symlink from
		// making a recursive removal reach outside the selected tree.
		below := slices.Clone(tree.nodes[1:])
		slices.SortStableFunc(below, func(a, b *store.Node) int {
			return strings.Count(b.URL.Path, "/") - strings.Count(a.URL.Path, "/")
		})
		for _, n := range below {
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(top.URL.Path, n.URL.Path)
			if err != nil {
				return fail(n, err)
			}
			fi, err := root.Lstat(rel)
			if err != nil {
				return fail(n, err)
			}
			if !unchangedLocal(n, fi) {
				return fail(n, errChanged)
			}
			if err := root.Remove(rel); err != nil {
				return fail(n, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err = parent.Lstat(base)
	if err != nil {
		return fail(top, err)
	}
	if !unchangedLocal(top, info) {
		return fail(top, errChanged)
	}
	if err := parent.Remove(base); err != nil {
		return fail(top, err)
	}
	return nil
}

func unchangedLocal(n *store.Node, info os.FileInfo) bool {
	if n.Info == nil || !os.SameFile(n.Info, info) || n.Info.Mode() != info.Mode() {
		return false
	}
	// Removing children changes directory times and sizes ourselves.
	return n.IsDir() || n.Size == info.Size() && n.ModTime.Equal(info.ModTime())
}

func (b *backend) mkdir(ctx context.Context, parent location, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return errors.New("enter a single directory name")
	}
	u := parent.url.Join(name)
	if existing, err := b.store(u).Stat(ctx, u, false); err == nil {
		if !existing.IsDir() {
			return fmt.Errorf("cannot create directory %s: a file already exists", u.Display())
		}
		return nil
	} else if !store.IsNotExist(err) {
		return err
	}
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
