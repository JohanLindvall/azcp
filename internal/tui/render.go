// SPDX-License-Identifier: MIT

package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"

	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/store"
)

// Names and previews are untrusted terminal text. Controls are rendered as
// visible replacements, and credentials go through the same redactor as logs.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return '�'
		}
		return r
	}, logx.Redact(s))
}

func (a *app) text(x, y, width int, s string, style tcell.Style) {
	if width <= 0 {
		return
	}
	g := uniseg.NewGraphemes(safeText(s))
	used := 0
	for g.Next() {
		w := g.Width()
		if used+w > width {
			break
		}
		r := g.Runes()
		if w > 0 && len(r) > 0 {
			a.screen.SetContent(x+used, y, r[0], r[1:], style)
			used += w
		}
	}
	for used < width {
		a.screen.SetContent(x+used, y, ' ', nil, style)
		used++
	}
}

func (a *app) right(x, y, width int, s string, style tcell.Style) {
	s = humanize.Elide(safeText(s), width)
	a.text(x, y, width, strings.Repeat(" ", max(0, width-humanize.Width(s)))+s, style)
}

func (a *app) frame(x, y, width, height int, style tcell.Style) {
	a.text(x, y, width, "╭"+strings.Repeat("─", width-2)+"╮", style)
	a.text(x, y+height-1, width, "╰"+strings.Repeat("─", width-2)+"╯", style)
	for row := y + 1; row < y+height-1; row++ {
		a.screen.SetContent(x, row, '│', nil, style)
		a.screen.SetContent(x+width-1, row, '│', nil, style)
	}
}

func (a *app) draw() {
	w, h := a.screen.Size()
	a.screen.Fill(' ', a.theme.base)
	a.screen.HideCursor()
	if w < 50 || h < 14 {
		a.text(0, 0, w, "azcp: enlarge terminal to at least 50 x 14", a.theme.accent)
		a.text(0, 1, w, "q quits; Esc cancels active work", a.theme.muted)
		logx.WithTerminal(a.screen.Show)
		return
	}
	a.text(1, 0, 4, "azcp", a.theme.accent.Bold(true))
	a.text(7, 0, 11, "Commander", a.theme.base.Bold(true))
	hint := "u: usage · s: sort · f: counts · z: calc · Tab: panes"
	if a.single {
		a.text(7, 0, 11, "Browser", a.theme.base.Bold(true))
		hint = "u: usage · s: sort · f: counts · z: calc · /: filter"
	}
	if w < 75 {
		hint = "u: usage · s: sort · z: calc"
	}
	if a.panes[a.active].usage != nil {
		a.text(7, 0, 11, "Disk usage", a.theme.base.Bold(true))
		hint = "s: sort · f: counts · a: disk/logical · u: panes · F1: help"
		if w < 80 {
			hint = "s: sort · a: size · u: panes"
		}
	}
	if a.backend.opt.DryRun {
		hint = "DRY RUN · " + hint
	}
	a.right(20, 0, w-21, hint, a.theme.muted)
	if a.panes[a.active].usage != nil {
		a.drawUsagePane(a.active, w, h-7)
	} else if a.single {
		a.drawPane(0, 0, w, h-7)
	} else {
		left := (w - 1) / 2
		a.drawPane(0, 0, left, h-7)
		a.drawPane(1, left+1, w-left-1, h-7)
	}
	p := &a.panes[a.active]
	if p.cursor >= 0 && p.cursor < len(p.entries) {
		e := p.entries[p.cursor]
		loc := e.loc
		if p.usage != nil && e.usage != nil {
			loc = p.usage.location(e.usage)
		}
		a.text(1, h-6, w-2, humanize.Elide(safeText(loc.display()), w-2), a.theme.muted)
	}
	a.drawStatus(w, h)
	if logs := a.logs.snapshot(); len(logs) > 0 {
		lines := wrap(logs[len(logs)-1], w-4)
		for i := 0; i < min(2, len(lines)); i++ {
			a.text(2, h-3+i, w-4, lines[i], a.theme.warn)
		}
	}
	labels := []string{"Help", "Root", "View", "Go", "Copy", "Move", "Mkdir", "Delete", "Logs", "Quit"}
	if a.single {
		labels[4], labels[5] = "", ""
	}
	for i, label := range labels {
		x, end := i*w/10, (i+1)*w/10
		key := fmt.Sprint(i + 1)
		if end-x < 8 && i == 7 {
			label = "Del"
		}
		a.text(x, h-1, len(key), key, a.theme.key)
		a.text(x+len(key)+1, h-1, end-x-len(key)-1, label, a.theme.base)
	}
	if a.modal != nil {
		a.drawDialog(w, h)
	}
	logx.WithTerminal(a.screen.Show)
}

func paneRows(height int) int { return max(1, height-11) }

func (a *app) drawPane(index, x, width, bottom int) {
	p := &a.panes[index]
	active := a.active == index
	border, pathStyle := a.theme.border, a.theme.muted
	kind := "LOCAL"
	if p.loc.url.IsRemote() {
		kind = "AZURE"
	}
	label := " " + kind + " "
	if active {
		border, pathStyle = a.theme.accent, a.theme.base.Bold(true)
		label = " ● " + kind + " "
	}
	label += "· " + p.sort.label() + " "
	a.frame(x, 1, width, bottom, border)
	a.text(x+2, 1, min(width-4, humanize.Width(label)), humanize.Elide(label, width-4), border.Bold(active))
	a.text(x+2, 2, width-4, humanize.Elide(safeText(p.loc.display()), width-4), pathStyle)
	sizeWidth, dateWidth := 9, 0
	// Both panes of a pair decide together, so their columns line up.
	if screenWidth, _ := a.screen.Size(); screenWidth >= 121 || a.single && width >= 60 {
		dateWidth = 14
	}
	nameWidth := width - 7 - sizeWidth - dateWidth
	sizeX := x + width - 2 - dateWidth - sizeWidth
	a.text(x+4, 3, nameWidth, "Name", a.theme.muted)
	column := "Size"
	if p.showCounts {
		column = "Files"
	}
	a.right(sizeX, 3, sizeWidth, column, a.theme.muted)
	if dateWidth > 0 {
		heading := "Modified"
		if p.sort.field == sortNewest {
			heading = "Newest"
		}
		a.right(sizeX+sizeWidth+1, 3, dateWidth-1, heading, a.theme.muted)
	}
	rows := bottom - 4
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+rows {
		p.offset = p.cursor - rows + 1
	}
	p.offset = max(0, p.offset)
	for row := 0; row < rows; row++ {
		i := p.offset + row
		if i >= len(p.entries) {
			break
		}
		e := p.entries[i]
		style, prefix := a.theme.base, "  "
		if p.selected[i] {
			style, prefix = a.theme.marked, " ●"
		}
		if i == p.cursor && active {
			style, prefix = a.theme.focus, "›"+prefix[1:]
		} else if i == p.cursor {
			prefix = "·" + prefix[1:]
		}
		name, size := e.name, ""
		if e.directory() {
			name += "/"
			size = "dir"
			if e.sizePending {
				size = "…"
			} else if e.dirSize != nil {
				size = humanize.Bytes(e.dirSize.bytes)
			}
			if e.usage != nil {
				size = p.usageText(e.usage, p.showCounts)
			}
			if !p.selected[i] && !(i == p.cursor && active) {
				style = style.Bold(true)
			}
		} else if e.node != nil || e.usage != nil {
			if e.usage != nil {
				size = p.usageText(e.usage, p.showCounts)
			} else {
				size = humanize.Bytes(e.node.Size)
			}
			if e.kind() == store.KindSymlink {
				name += "@"
			}
		}
		if p.showCounts {
			if count, known := e.fileCount(); known {
				size = humanize.Count(count)
			}
		}
		y := row + 4
		a.text(x+1, y, width-2, "", style)
		a.text(x+1, y, 2, prefix, style)
		a.text(x+4, y, nameWidth, humanize.Elide(safeText(name), nameWidth), style)
		a.right(sizeX, y, sizeWidth, size, style)
		if when := p.sort.when(e); dateWidth > 0 && !when.IsZero() {
			a.right(sizeX+sizeWidth+1, y, dateWidth-1, when.Local().Format("Jan 02 15:04"), style)
		}
	}
	marked := 0
	for _, yes := range p.selected {
		if yes {
			marked++
		}
	}
	n := int64(max(0, len(p.entries)-1))
	count := fmt.Sprintf(" %d %s ", n, humanize.Plural(n, "entry", "entries"))
	if p.listFilter.active() {
		count = fmt.Sprintf(" %d of %d shown · filter: %s ", n, n+int64(len(p.hidden)), p.listFilter.text)
	}
	selected := fmt.Sprintf(" %d marked ", marked)
	if marked > 0 && humanize.Width(count)+humanize.Width(selected)+6 > width {
		a.text(x+2, bottom, width-4, selected, a.theme.marked)
		return
	}
	count = humanize.Elide(safeText(count), width-4-humanize.Width(selected)*min(marked, 1))
	a.text(x+2, bottom, humanize.Width(count), count, a.theme.muted)
	if marked > 0 {
		a.text(x+width-2-humanize.Width(selected), bottom, humanize.Width(selected), selected, a.theme.marked)
	}
}

func wrap(s string, width int) []string {
	var lines []string
	var line strings.Builder
	used := 0
	g := uniseg.NewGraphemes(safeText(s))
	for g.Next() {
		w := g.Width()
		if used+w > width && used > 0 {
			lines = append(lines, line.String())
			line.Reset()
			used = 0
		}
		line.WriteString(g.Str())
		used += w
	}
	return append(lines, line.String())
}
