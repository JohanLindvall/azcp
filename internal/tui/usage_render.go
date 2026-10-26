// SPDX-License-Identifier: MIT

package tui

import (
	"fmt"
	"strings"

	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/store"
)

func (p *pane) usageLabel(counts bool) string {
	if counts {
		return "Files"
	}
	if p.loc.url.IsRemote() {
		return "Stored"
	}
	if p.sort.allocated {
		return "Disk"
	}
	return "Logical"
}

func (p *pane) usageValue(n *usageNode, counts bool) (int64, bool) {
	e := entry{usage: n}
	if counts {
		return e.fileCount()
	}
	return e.usageSize(p.sort.allocated)
}

func (p *pane) usageText(n *usageNode, counts bool) string {
	if n.excluded {
		return "skip"
	}
	num, known := p.usageValue(n, counts)
	if !known {
		return "?"
	}
	if counts {
		return humanize.Count(num)
	}
	return humanize.Bytes(num)
}

func (a *app) drawUsagePane(index, width, bottom int) {
	p := &a.panes[index]
	a.frame(0, 1, width, bottom, a.theme.accent)
	label := " USAGE · " + p.sort.label() + " "
	if p.sort.field == sortSize {
		label = " USAGE · " + strings.Replace(p.sort.label(), "Size", p.usageLabel(false), 1) + " "
	}
	a.text(2, 1, humanize.Width(label), label, a.theme.accent.Bold(true))
	a.text(2, 2, width-4, humanize.Elide(safeText(p.loc.display()), width-4), a.theme.base.Bold(true))
	metricX := width - 11
	extraX, shareX, graphX := 0, metricX-8, 0
	if width >= 90 {
		extraX = metricX - 10
		shareX = extraX - 8
	}
	nameWidth := shareX - 5
	if width >= 70 {
		graphX = shareX - 11
		nameWidth = graphX - 5
	}
	a.text(4, 3, nameWidth, "Name", a.theme.muted)
	a.right(metricX, 3, 9, p.usageLabel(p.showCounts), a.theme.muted)
	if extraX > 0 {
		a.right(extraX, 3, 9, p.usageLabel(!p.showCounts), a.theme.muted)
	}
	a.right(shareX, 3, 7, "%", a.theme.muted)
	if graphX > 0 {
		a.text(graphX, 3, 10, "Usage", a.theme.muted)
	}
	var largest int64
	for _, n := range p.usageDir.children {
		if size, ok := p.usageValue(n, p.showCounts); ok {
			largest = max(largest, size)
		}
	}
	total, totalKnown := p.usageValue(p.usageDir, p.showCounts)
	rows := bottom - 4
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+rows {
		p.offset = p.cursor - rows + 1
	}
	p.offset = max(0, p.offset)
	for row := 0; row < rows && row+p.offset < len(p.entries); row++ {
		i := row + p.offset
		e := p.entries[i]
		style, prefix := a.theme.base, "  "
		if p.selected[i] {
			style, prefix = a.theme.marked, " ●"
		}
		if i == p.cursor {
			style, prefix = a.theme.focus, "›"+prefix[1:]
		}
		name := e.name
		if e.directory() {
			name += "/"
		} else if e.kind() == store.KindSymlink {
			name += "@"
		}
		y := row + 4
		a.text(1, y, width-2, "", style)
		a.text(1, y, 2, prefix, style)
		a.text(4, y, nameWidth, humanize.Elide(safeText(name), nameWidth), style)
		if i == 0 || e.usage == nil {
			continue
		}
		n := e.usage
		flag := " "
		switch {
		case n.excluded:
			flag = ">"
		case n.total.incomplete || n.total.diskUnknown && p.sort.allocated:
			flag = "!"
		case n.link != nil:
			flag = "H"
		case n.kind == store.KindDir:
			style = style.Bold(true)
		}
		a.text(3, y, 1, flag, style)
		a.right(metricX, y, 9, p.usageText(n, p.showCounts), style)
		if extraX > 0 {
			a.right(extraX, y, 9, p.usageText(n, !p.showCounts), style)
		}
		value, known := p.usageValue(n, p.showCounts)
		share := "—"
		if known && totalKnown && total > 0 {
			share = fmt.Sprintf("%.1f%%", 100*float64(value)/float64(total))
		}
		a.right(shareX, y, 7, share, style)
		if graphX > 0 && known && largest > 0 {
			filled := int(10 * float64(value) / float64(largest))
			if value > 0 {
				filled = max(1, filled)
			}
			a.text(graphX, y, 10, strings.Repeat("█", min(10, filled)), style)
		}
	}
	dirs := humanize.Count(p.usageDir.total.dirs)
	if p.usageDir.total.incomplete {
		dirs = "?"
	}
	footer := fmt.Sprintf(" %s: %s · Files: %s · Dirs: %s ", p.usageLabel(false),
		p.usageText(p.usageDir, false), p.usageText(p.usageDir, true), dirs)
	if !p.loc.url.IsRemote() && p.usage.oneFS {
		footer += "· one filesystem "
	}
	if p.filter != "" {
		footer += "· filter: " + p.filter + " (totals include hidden entries) "
	}
	if p.usageDir.total.excluded > 0 {
		footer += fmt.Sprintf("· %d excluded ", p.usageDir.total.excluded)
	}
	marked := 0
	for _, yes := range p.selected {
		if yes {
			marked++
		}
	}
	if marked > 0 {
		footer += fmt.Sprintf("· %d marked ", marked)
	}
	a.text(2, bottom, width-4, humanize.Elide(safeText(footer), width-4), a.theme.muted)
}
