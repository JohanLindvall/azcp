package engine

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// --delete makes the destination match the source, which means removing things.
// It is the only operation here that destroys data, so it is deliberately
// timid:
//
//   - Nothing is deleted unless the whole copy succeeded. A listing that failed
//     half way through looks exactly like a source with fewer files in it, and
//     that is precisely when deleting would be catastrophic.
//   - Anything --exclude ruled out is protected rather than deleted. An
//     exclusion says "this is not my business", not "remove it".
//   - --dry-run reports every deletion without making one.
//
// The set of things to keep is built while planning, from what the source
// actually offered, rather than by comparing two listings afterwards.

// pruner records what a copy put at each destination root, so that whatever
// else is there can be removed.
type pruner struct {
	mu    sync.Mutex
	roots map[string]*uri.URL // path -> root
	keep  map[string]map[string]bool
}

func newPruner() *pruner {
	return &pruner{roots: map[string]*uri.URL{}, keep: map[string]map[string]bool{}}
}

// root registers a destination that a recursive copy is filling.
func (p *pruner) root(dst *uri.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := destinationKey(dst)
	if _, seen := p.roots[k]; !seen {
		p.roots[k] = dst
		p.keep[k] = map[string]bool{}
	}
}

// wrote records that the source provides this path, relative to a root.
func (p *pruner) wrote(root *uri.URL, rel string) {
	if rel == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.keep[destinationKey(root)]; ok {
		m[rel] = true
	}
}

// prune removes everything under the registered roots that the source did not
// provide. It reports how many entries were removed.
func (e *Engine) prune(ctx context.Context) int64 {
	if e.pruner == nil || len(e.pruner.roots) == 0 {
		return 0
	}
	if n := e.failed.Load(); n > 0 {
		e.note("not deleting anything at the destination: %d file(s) failed to "+
			"copy, so what the source holds is not known", n)
		return 0
	}
	if err := ctx.Err(); err != nil {
		e.note("not deleting anything at the destination: the copy was interrupted")
		return 0
	}

	var removed int64
	// Roots in a fixed order, so a --dry-run reads the same way twice.
	for _, key := range slices.Sorted(maps.Keys(e.pruner.roots)) {
		root, keep := e.pruner.roots[key], e.pruner.keep[key]
		var extra []*store.Node

		reported := false
		onError := func(u *uri.URL, err error) error {
			reported = true
			e.fail("cannot read %s to work out what to delete: %s",
				quote(u.Display()), brief(err))
			return err
		}
		namespace := e.storeFor(root)
		if !root.IsRemote() {
			// -L describes the source. Following a destination link here would
			// let --delete remove data outside the destination tree.
			namespace = local.New(e.log, false)
		}
		walkErr := namespace.WalkAll(ctx, root, onError, func(n *store.Node) error {
			rel, ok := relativeDestination(root, n.URL)
			if !ok || rel == "" {
				return nil
			}
			// An excluded entry is none of this copy's business; deleting it
			// would go well beyond what the exclusion asked for.
			if keep[rel] || (e.filter.active() && !e.filter.allow(rel)) {
				// A retained child also retains every directory containing it.
				// The walk may already have queued those directories as extras.
				keep[rel] = true
				for parent := rel; ; {
					i := strings.LastIndexByte(parent, '/')
					if i < 0 {
						break
					}
					parent = parent[:i]
					keep[parent] = true
				}
				e.log.Debug("not deleting an excluded entry", "path", n.URL.Display())
				return nil
			}
			extra = append(extra, n)
			return nil
		})
		if walkErr != nil {
			if e.opt.DryRun && store.IsNotExist(walkErr) {
				continue
			}
			if !reported && !interrupted(ctx, walkErr) {
				e.fail("cannot read %s to work out what to delete: %s", quote(root.Display()), brief(walkErr))
			}
			e.note("not deleting anything under %s: it could not be listed in full",
				quote(root.Display()))
			continue
		}

		// Deepest first, so a directory is empty by the time it is removed.
		depth := func(n *store.Node) int { return strings.Count(n.URL.PathPart(), "/") }
		slices.SortStableFunc(extra, func(a, b *store.Node) int { return cmp.Compare(depth(b), depth(a)) })
		for _, n := range extra {
			if ctx.Err() != nil {
				return removed
			}
			if rel, ok := relativeDestination(root, n.URL); ok && keep[rel] {
				continue
			}
			if e.opt.DryRun {
				e.reportRemoval(n.URL, true)
				removed++
				continue
			}
			var err error
			if n.IsDir() && n.URL.IsRemote() {
				err = e.az.RemoveMarker(ctx, n.URL)
			} else {
				err = e.storeFor(n.URL).Remove(ctx, n.URL)
			}
			if err != nil {
				if store.IsNotExist(err) {
					continue
				}
				e.fail("cannot remove %s: %s", quote(n.URL.Display()), brief(err))
				continue
			}
			if e.opt.Verbose || e.opt.Output == cli.OutputJSON {
				e.reportRemoval(n.URL, false)
			}
			e.log.Debug("removed", "path", n.URL.Display())
			removed++
		}
	}
	return removed
}

func (e *Engine) reportRemoval(u *uri.URL, dryRun bool) {
	if e.opt.Output == cli.OutputJSON {
		event := "remove"
		if dryRun {
			event = "would-remove"
		}
		logx.Printf("%s\n", jsonLine(map[string]any{"event": event, "destination": u.Display()}))
		return
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	logx.Printf("%s %s\n", verb, quote(u.Display()))
}
