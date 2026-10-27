// SPDX-License-Identifier: MIT

package tui

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/store"
)

type sortField int

const (
	sortName sortField = iota
	sortModified
	sortSize
	sortExtension
	sortType
	sortFiles
	sortNewest
)

type paneSort struct {
	field          sortField
	reverse        bool
	mixDirectories bool
	allocated      bool
}

func (s paneSort) label() string {
	names := [...]string{"Name", "Modified", "Size", "Extension", "Type", "Files", "Newest"}
	arrow := "↑"
	if s.reverse {
		arrow = "↓"
	}
	return names[s.field] + " " + arrow
}

func (e entry) size() (int64, bool) {
	return e.usageSize(false)
}

func (e entry) usageSize(allocated bool) (int64, bool) {
	if e.usage != nil {
		t := e.usage.total
		if allocated {
			return t.disk, !t.incomplete && !t.diskUnknown && !e.usage.excluded
		}
		return t.logical, !t.incomplete && !e.usage.excluded
	}
	if e.directory() {
		if e.dirSize != nil {
			return e.dirSize.bytes, true
		}
		return 0, false
	}
	if e.node == nil || e.node.Size < 0 {
		return 0, false
	}
	return e.node.Size, true
}

func (e entry) fileCount() (int64, bool) {
	if e.usage != nil {
		return e.usage.total.files, !e.usage.total.incomplete && !e.usage.excluded
	}
	if e.directory() {
		if e.dirSize != nil {
			return e.dirSize.files, true
		}
		return 0, false
	}
	if e.node.IsRegular() {
		return 1, true
	}
	return 0, true
}

// newest is the latest modification of a file anywhere below a directory,
// once its totals are known, and a file's own modification time. A container
// or directory's own timestamp says nothing about when its contents changed.
func (e entry) newest() time.Time {
	switch {
	case e.usage != nil && e.usage.kind == store.KindDir:
		return e.usage.total.newest
	case e.usage != nil:
		return e.usage.modified
	case !e.directory():
		return e.modified()
	case e.dirSize != nil:
		return e.dirSize.newest
	}
	return time.Time{}
}

// when is the time the pane shows and sorts by.
func (s paneSort) when(e entry) time.Time {
	if s.field == sortNewest {
		return e.newest()
	}
	return e.modified()
}

func extension(e entry) string {
	if e.directory() {
		return ""
	}
	i := strings.LastIndexByte(e.name, '.')
	if i <= 0 {
		return ""
	}
	return strings.ToLower(e.name[i+1:])
}

func (s paneSort) compare(a, b entry) int {
	if !s.mixDirectories && a.directory() != b.directory() {
		if a.directory() {
			return -1
		}
		return 1
	}
	n := 0
	switch s.field {
	case sortModified, sortNewest:
		at, bt := s.when(a), s.when(b)
		ak, bk := !at.IsZero(), !bt.IsZero()
		if ak != bk {
			return knownFirst(ak)
		}
		if ak {
			n = at.Compare(bt)
		}
	case sortSize, sortFiles:
		as, ak := a.usageSize(s.allocated)
		bs, bk := b.usageSize(s.allocated)
		if s.field == sortFiles {
			as, ak = a.fileCount()
			bs, bk = b.fileCount()
		}
		// An unmeasured directory is not empty, in either direction.
		if ak != bk {
			return knownFirst(ak)
		}
		if ak {
			n = cmp.Compare(as, bs)
		}
	case sortExtension:
		n = strings.Compare(extension(a), extension(b))
	case sortType:
		n = strings.Compare(a.kind().String(), b.kind().String())
	}
	if n == 0 {
		n = strings.Compare(a.name, b.name)
	}
	if s.reverse {
		return -n
	}
	return n
}

func knownFirst(known bool) int {
	if known {
		return -1
	}
	return 1
}

func (p *pane) sortEntries() {
	if len(p.entries) < 2 {
		return
	}
	// Marks and the cursor belong to entries, not row numbers. Sorting a
	// permutation also distinguishes a blob and prefix sharing the same name.
	order := make([]int, len(p.entries)-1)
	for i := range order {
		order[i] = i + 1
	}
	slices.SortStableFunc(order, func(i, j int) int { return p.sort.compare(p.entries[i], p.entries[j]) })
	entries := make([]entry, len(p.entries))
	entries[0] = p.entries[0]
	marked := make(map[int]bool)
	cursor := 0
	for row, old := range order {
		i := row + 1
		entries[i] = p.entries[old]
		if p.selected[old] {
			marked[i] = true
		}
		if old == p.cursor {
			cursor = i
		}
	}
	p.entries, p.selected, p.cursor = entries, marked, cursor
}

func (a *app) sortKey(ev *tcell.EventKey) {
	p := &a.panes[a.active]
	switch {
	case ev.Key() == tcell.KeyEnter:
		a.modal = nil
		return
	case ev.Rune() >= '1' && ev.Rune() <= '7':
		p.sort.field = sortField(ev.Rune() - '1')
		if p.sort.field == sortSize || p.sort.field == sortFiles {
			p.showCounts = p.sort.field == sortFiles
		}
	case ev.Rune() == 'r':
		p.sort.reverse = !p.sort.reverse
	case ev.Rune() == 'd':
		p.sort.mixDirectories = !p.sort.mixDirectories
	case ev.Rune() == 'f':
		a.toggleCounts()
		return
	default:
		return
	}
	p.sortEntries()
	a.status = "Sorted by " + p.sort.label()
}

func (a *app) toggleCounts() {
	p := &a.panes[a.active]
	p.showCounts = !p.showCounts
	a.status = "Showing sizes; z/Z calculates directory totals"
	if p.showCounts {
		a.status = "Showing recursive file counts; z/Z calculates directory totals"
	}
}

func (p *pane) sortMenu() []string {
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	column := "sizes"
	if p.showCounts {
		column = "file counts"
	}
	return []string{
		"1 Name   2 Modified   3 Size",
		"4 Extension  5 Type  6 Files  7 Newest",
		"Current: " + p.sort.label(),
		fmt.Sprintf("r Reverse: %s   d Dirs first: %s", onOff(p.sort.reverse), onOff(!p.sort.mixDirectories)),
		"f Column: " + column + " (toggle)",
	}
}
