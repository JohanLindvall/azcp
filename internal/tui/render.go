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
)

var (
	baseStyle   = tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorNavy)
	barStyle    = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorSilver)
	activeStyle = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorTeal).Bold(true)
	markStyle   = baseStyle.Foreground(tcell.ColorYellow)
	lineStyle   = baseStyle.Foreground(tcell.ColorAqua)
	dialogStyle = tcell.StyleDefault.Foreground(tcell.ColorBlack).Background(tcell.ColorSilver)
)

// Names and previews are untrusted terminal text. Controls are rendered as
// visible replacements, and credentials go through the same redactor as logs.
func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u202e' || r == '\u202d' || r >= '\u2066' && r <= '\u2069' {
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

func (a *app) draw() {
	w, h := a.screen.Size()
	a.screen.Fill(' ', baseStyle)
	a.screen.HideCursor()
	if w < 50 || h < 14 {
		a.text(0, 0, w, "azcp: enlarge terminal to at least 50 x 14", barStyle)
		a.text(0, 1, w, "q quits; Esc cancels active work", barStyle)
		logx.WithTerminal(a.screen.Show)
		return
	}
	title := " azcp Commander   Tab: switch pane   Space: select"
	if a.backend.opt.DryRun {
		title += "   DRY RUN"
	}
	a.text(0, 0, w, title, barStyle)
	left := w / 2
	a.drawPane(0, 0, left, h-6)
	a.drawPane(1, left, w-left, h-6)
	p := &a.panes[a.active]
	if p.cursor >= 0 && p.cursor < len(p.entries) {
		e := p.entries[p.cursor]
		detail := e.loc.display()
		if e.loc.url != nil {
			detail = e.loc.url.Display()
		}
		a.text(0, h-5, w, detail, lineStyle)
	}
	status := a.status
	if a.busy != nil {
		done, failed, _, _, bytes, _ := a.busy.progress.Totals()
		status = fmt.Sprintf("%s | %d done, %d failed, %s | Esc: cancel", a.busy.label, done, failed, humanize.Bytes(bytes))
	}
	a.text(0, h-4, w, status, barStyle)
	if logs := a.logs.snapshot(); len(logs) > 0 {
		lines := wrap(logs[len(logs)-1], w)
		for i := 0; i < min(2, len(lines)); i++ {
			a.text(0, h-3+i, w, lines[i], markStyle)
		}
	}
	labels := []string{"1 Help", "2 Root", "3 View", "4 Go", "5 Copy", "6 Move", "7 Mkdir", "8 Delete", "9 Logs", "10 Quit"}
	for i, label := range labels {
		x, end := i*w/10, (i+1)*w/10
		a.text(x, h-1, end-x, label, barStyle)
	}
	if a.modal != nil {
		a.drawDialog(w, h)
	}
	logx.WithTerminal(a.screen.Show)
}

func (a *app) drawPane(index, x, width, bottom int) {
	p := &a.panes[index]
	header := lineStyle
	if a.active == index {
		header = activeStyle
	}
	a.text(x, 1, width, " "+humanize.Elide(safeText(p.loc.display()), width-2), header)
	a.text(x, 2, width, " Name", lineStyle)
	rows := bottom - 3
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
		style := baseStyle
		prefix := " "
		if p.selected[i] {
			style, prefix = markStyle, "*"
		}
		if i == p.cursor && a.active == index {
			style = activeStyle
		}
		name, size := e.name, ""
		if e.directory() {
			name += "/"
			size = "<DIR>"
		} else if e.node != nil {
			size = humanize.Bytes(e.node.Size)
			if e.node.IsSymlink() {
				name += "@"
			}
		}
		nameWidth := max(1, width-12)
		a.text(x, row+3, width, "", style)
		a.text(x, row+3, nameWidth, prefix+humanize.Truncate(safeText(name), nameWidth-1), style)
		a.text(x+nameWidth, row+3, width-nameWidth-1, size, style)
	}
	marked := 0
	for _, yes := range p.selected {
		if yes {
			marked++
		}
	}
	a.text(x, bottom, width, fmt.Sprintf(" %d entries | %d selected", max(0, len(p.entries)-1), marked), header)
	for y := 2; y < bottom; y++ {
		a.screen.SetContent(x+width-1, y, '│', nil, lineStyle)
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

func (a *app) drawDialog(w, h int) {
	d := a.modal
	width := min(w-4, 96)
	var content []string
	footer := "Esc: close"
	switch d.kind {
	case "view":
		for _, line := range d.lines {
			content = append(content, wrap(strings.ReplaceAll(line, "\t", "    "), width-4)...)
		}
		footer = "Arrows/PgUp/PgDn: scroll   Enter/Esc: close"
	case "input":
		if d.action == "Copy" || d.action == "Move" {
			content = append(content, wrap(fmt.Sprintf("%d entries. Empty input uses: %s", len(d.source), d.target.display()), width-4)...)
		}
		content = append(content, "", "> "+humanize.Elide(safeText(d.text), width-6), "")
		footer = "Enter: continue   Ctrl-U: clear   Esc: cancel"
	case "confirm":
		content = append(content, wrap(fmt.Sprintf("%s %d entries and their directory contents?", d.action, len(d.source)), width-4)...)
		if d.action == "Delete" {
			content = append(content, "Deletion is permanent.")
		} else {
			content = append(content, "Existing files will be replaced.")
			content = append(content, wrap("To: "+d.target.display(), width-4)...)
		}
		if a.backend.opt.DryRun {
			content = append(content, "DRY RUN: no files will change.")
		}
		for _, e := range d.source[:min(5, len(d.source))] {
			content = append(content, wrap("  "+e.loc.display(), width-4)...)
		}
		if len(d.source) > 5 {
			content = append(content, fmt.Sprintf("  ...and %d more", len(d.source)-5))
		}
		footer = "y: confirm   n / Enter / Esc: cancel"
	}
	height := min(h-4, len(content)+4)
	x, y := (w-width)/2, (h-height)/2
	for row := 0; row < height; row++ {
		a.text(x, y+row, width, "", dialogStyle)
	}
	a.text(x, y, width, " "+d.title, activeStyle)
	rows := height - 3
	d.scroll = min(max(0, d.scroll), max(0, len(content)-rows))
	for i := 0; i < rows && i+d.scroll < len(content); i++ {
		a.text(x+2, y+1+i, width-4, content[i+d.scroll], dialogStyle)
	}
	a.text(x, y+height-1, width, footer, activeStyle)
}
