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
	a.start("Calculating directory totals", func(ctx context.Context, progress *progress.Reporter) (any, error) {
		return a.backend.directorySizes(ctx, base, selected, all, progress)
	}, func(r jobResult) {
		for _, row := range indices {
			p.entries[row].sizePending = false
		}
		if r.err == nil {
			sizes := r.value.([]directorySize)
			var total directorySize
			for i, row := range indices {
				p.entries[row].dirSize = &sizes[i]
				total.bytes += sizes[i].bytes
				total.files += sizes[i].files
			}
			a.status = fmt.Sprintf("%d directories: %s in %s files (logical sizes)", len(sizes), humanize.Bytes(total.bytes), humanize.Count(total.files))
		}
		p.sortEntries()
	})
	a.busy.sizing = true
}

// directorySizes adds metadata lengths, never reads file bodies, and retains
// only one total per selected directory. A failed or cancelled walk supplies
// no totals: a partial listing must not masquerade as an empty or small tree.
func (b *backend) directorySizes(ctx context.Context, base location, selected []entry, all bool, p *progress.Reporter) ([]directorySize, error) {
	sizes := make([]directorySize, len(selected))
	p.SetScanning(true)
	var total int64
	add := func(i int, n *store.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i < 0 || !n.IsRegular() {
			return nil
		}
		if n.Size < 0 || n.Size > math.MaxInt64-total {
			return errors.New("directory size exceeds the supported range")
		}
		sizes[i].bytes += n.Size
		sizes[i].files++
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
					return add(i, n)
				}
			}
			return ctx.Err()
		})
		if err != nil {
			return nil, err
		}
		return sizes, nil
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
			if u.IsRemote() && base.url.Container != "" && !strings.HasPrefix(n.URL.Key, u.Key+"/") {
				return ctx.Err()
			}
			return add(i, n)
		}); err != nil {
			return nil, err
		}
	}
	return sizes, nil
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
