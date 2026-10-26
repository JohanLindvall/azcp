// SPDX-License-Identifier: MIT

package tui

import (
	"fmt"
	"strings"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

func (a *app) drawDialog(w, h int) {
	d := a.modal
	width := min(w-8, 94)
	inner := width - 6
	var content []string
	footer, field := "Esc: close", -1
	switch d.kind {
	case "sort":
		content = a.panes[a.active].sortMenu()
		footer = "Enter/Esc: close"
	case "view":
		for _, line := range d.lines {
			content = append(content, wrap(strings.ReplaceAll(line, "\t", "    "), inner)...)
		}
		footer = "↑↓ / PgUp/PgDn: scroll   Enter/Esc: close"
		if inner < 42 {
			footer = "↑↓: scroll  Enter/Esc: close"
		}
	case "input":
		if d.action == "Copy" || d.action == "Move" {
			content = append(content, fmt.Sprintf("%d %s · leave empty to keep:", len(d.source), humanize.Plural(int64(len(d.source)), "entry", "entries")),
				humanize.Elide(safeText(d.target.display()), inner), "")
		}
		field = len(content)
		content = append(content, "> "+humanize.Elide(safeText(d.text), inner-3), "")
		footer = "Enter: use  Ctrl-U: clear  Esc: cancel"
	case "confirm":
		content = append(content, wrap(fmt.Sprintf("%s %d %s (including contents)?", d.action, len(d.source), humanize.Plural(int64(len(d.source)), "entry", "entries")), inner)...)
		if d.action == "Delete" {
			content = append(content, "Deletion is permanent.")
		} else {
			content = append(content, "To: "+humanize.Elide(safeText(d.target.display()), inner-4),
				"Existing files will be replaced.")
		}
		if a.backend.opt.DryRun {
			content = append(content, "DRY RUN: no files will change.")
		}
		content = append(content, "")
		for _, e := range d.source[:min(5, len(d.source))] {
			content = append(content, wrap("  "+e.loc.display(), inner)...)
		}
		if len(d.source) > 5 {
			content = append(content, fmt.Sprintf("  …and %d more", len(d.source)-5))
		}
		footer = "y: confirm   n / Enter / Esc: cancel"
		if d.action == "Copy" || d.action == "Move" {
			footer = "y: confirm  e: edit  Enter/Esc: cancel"
			if inner < 42 {
				footer = "y: yes  e: edit  Enter/Esc: cancel"
			}
		}
	}
	footerLines := wrap(footer, inner)
	height := min(h-4, len(content)+len(footerLines)+4)
	x, y := (w-width)/2, (h-height)/2
	for row := 0; row < height; row++ {
		a.text(x, y+row, width, "", a.theme.dialog)
	}
	fg, _, _ := a.theme.accent.Decompose()
	if d.action == "Delete" {
		fg, _, _ = a.theme.bad.Decompose()
	}
	border := a.theme.dialog.Foreground(fg)
	a.frame(x, y, width, height, border)
	title := " " + humanize.Elide(safeText(d.title), width-6) + " "
	a.text(x+2, y, humanize.Width(title), title, border.Bold(true))
	rows := height - len(footerLines) - 4
	d.scroll = min(max(0, d.scroll), max(0, len(content)-rows))
	for i := 0; i < rows && i+d.scroll < len(content); i++ {
		style := a.theme.dialog
		if i+d.scroll == field {
			style = a.theme.input
		}
		a.text(x+3, y+2+i, inner, content[i+d.scroll], style)
		if i+d.scroll == field {
			a.screen.ShowCursor(x+3+min(inner-1, humanize.Width(content[field])), y+2+i)
		}
	}
	for i, line := range footerLines {
		a.text(x+3, y+height-len(footerLines)-1+i, inner, line, border.Bold(true))
	}
}
