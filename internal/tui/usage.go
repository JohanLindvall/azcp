// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/progress"
)

func (p *pane) usagePath() string {
	if p.usage == nil || p.usageDir == p.usage.root {
		return ""
	}
	return p.usageDir.relative() + "/"
}

func (p *pane) usageEntries(focus *usageNode) {
	p.loc = p.usage.location(p.usageDir)
	parent := entry{name: "..", loc: parentLocation(p.loc)}
	if p.usageDir.parent != nil {
		parent.loc = p.usage.location(p.usageDir.parent)
		parent.usage = p.usageDir.parent
	}
	p.entries = make([]entry, 1, len(p.usageDir.children)+1)
	p.entries[0] = parent
	p.cursor, p.offset, p.selected = 0, 0, make(map[int]bool)
	for _, n := range p.usageDir.children {
		if !p.filter.match(n.name) {
			continue
		}
		name := n.name
		if name == "" {
			name = "/"
		}
		// Materialize full store nodes and credential-bearing URLs only for
		// operations, not every row of a directory containing millions of files.
		p.entries = append(p.entries, entry{name: name, usage: n})
		if n == focus {
			p.cursor = len(p.entries) - 1
		}
	}
	p.sortEntries()
	if p.cursor == 0 && len(p.entries) > 1 {
		p.cursor = 1
	}
}

func (p *pane) leaveUsage() {
	if p.usage == nil {
		return
	}
	p.usage, p.usageDir, p.filter = nil, nil, nameFilter{}
	p.sort, p.showCounts = p.savedSort, p.savedCounts
}

func (a *app) toggleUsage() {
	p := &a.panes[a.active]
	if p.usage == nil {
		a.scanUsagePane(a.active, p.loc, "", false)
		return
	}
	p.leaveUsage()
	a.navigate(p.loc)
}

func (a *app) scanUsagePane(i int, base location, current string, oneFS bool) {
	a.start("Scanning directory usage", func(ctx context.Context, progress *progress.Reporter) (any, error) {
		return a.backend.scanUsage(ctx, base, oneFS, progress)
	}, func(r jobResult) {
		if r.err == nil {
			a.setUsage(i, r.value.(*usageTree), current)
		} else if !errors.Is(r.err, context.Canceled) && a.panes[i].usage != nil {
			loc := a.panes[i].loc
			a.panes[i].leaveUsage()
			a.setEntries(i, loc, nil)
		}
	})
	a.busy.sizing = true
}

func (a *app) setUsage(i int, tree *usageTree, current string) {
	p := &a.panes[i]
	if p.usage == nil {
		p.savedSort, p.savedCounts = p.sort, p.showCounts
		p.sort = paneSort{field: sortSize, reverse: true, mixDirectories: true, allocated: !tree.base.url.IsRemote()}
		p.showCounts = false
	}
	p.usage, p.usageDir = tree, tree.find(current)
	p.usageEntries(nil)
	a.status = "Scan complete · r: rescan · u: two panes"
	if tree.root.total.incomplete {
		a.status = "Scan incomplete; ? marks incomplete totals · i: details · r: retry"
	}
	if tree.issueCount > 0 {
		a.status = fmt.Sprintf("Scan incomplete: %d errors; ? marks incomplete totals · i: details · r: retry", tree.issueCount)
	} else if tree.root.total.diskUnknown && !tree.root.total.incomplete {
		a.status = "Disk allocation unavailable for some entries · a: logical sizes · i: details"
	}
	for _, issue := range tree.issues {
		a.logs.add(issue)
	}
}

func (a *app) openUsage(n *usageNode) {
	if n == nil {
		return
	}
	p := &a.panes[a.active]
	if n.excluded {
		a.status = "Excluded: " + n.problem
		return
	}
	p.usageDir = n
	p.filter = nameFilter{}
	p.usageEntries(nil)
}

func (a *app) usageParent() {
	p := &a.panes[a.active]
	if p.usageDir.parent == nil {
		a.status = "At the scan root · g: scan another directory · u: return to two panes"
		return
	}
	child := p.usageDir
	p.usageDir, p.filter = child.parent, nameFilter{}
	p.usageEntries(child)
}

func (a *app) usageInfo() {
	p := &a.panes[a.active]
	n := p.usageDir
	if p.cursor > 0 && p.cursor < len(p.entries) {
		n = p.entries[p.cursor].usage
	}
	if n == nil {
		return
	}
	disk := fmt.Sprintf("%s (%d bytes)", humanize.Bytes(n.total.disk), n.total.disk)
	if n.total.diskUnknown {
		disk = "unavailable on part or all of this tree"
	}
	lines := []string{
		p.usage.location(n).display(), "",
		"Type: " + n.kind.String(),
		fmt.Sprintf("Logical size: %s (%d bytes)", humanize.Bytes(n.total.logical), n.total.logical),
		"Disk allocation: " + disk,
		fmt.Sprintf("Regular file names: %d · Subdirectories: %d", n.total.files, n.total.dirs),
		"Mode: " + n.mode.String(),
	}
	if p.loc.url.IsRemote() {
		lines[4] = "Azure reports stored blob lengths, not physical disk allocation."
	}
	if !n.modified.IsZero() {
		lines = append(lines, "Modified: "+n.modified.Local().Format(time.RFC3339))
	}
	lines = append(lines, "Scanned: "+p.usage.scanned.Local().Format(time.RFC3339))
	if n.link != nil {
		lines = append(lines, fmt.Sprintf("Hard links: %d · Each directory counts shared file data once.", n.link.count),
			"Deleting one name may leave the data reachable through another link.")
	}
	if n.problem != "" {
		lines = append(lines, "", "Problem: "+n.problem)
	}
	if n.total.incomplete {
		lines[3] += " (incomplete)"
		lines[4] += " (incomplete)"
		lines[5] += " (incomplete)"
		lines = append(lines, "Incomplete scan: totals are not complete. Refresh with r to retry.")
	}
	if n.total.excluded > 0 {
		lines = append(lines, fmt.Sprintf("Excluded entries: %d; their contents were not scanned.", n.total.excluded))
	}
	a.modal = &dialog{kind: "view", title: "Usage details", lines: lines}
}

func (a *app) refreshOtherPane(i int) {
	// A mutation invalidates a second cached view of overlapping data. Leave
	// it as a live directory listing rather than silently rescanning a large
	// independent tree after every deletion from the active usage view.
	p := &a.panes[i]
	p.leaveUsage()
	loc := p.loc
	a.start("Refreshing other pane", func(ctx context.Context, _ *progress.Reporter) (any, error) {
		return a.backend.list(ctx, loc)
	}, func(r jobResult) {
		if r.err == nil {
			a.setEntries(i, loc, r.value.([]entry))
		} else {
			a.setEntries(i, loc, nil)
		}
	})
}
