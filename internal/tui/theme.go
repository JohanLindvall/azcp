// SPDX-License-Identifier: MIT

package tui

import (
	"os"

	"github.com/gdamore/tcell/v2"
)

type theme struct {
	base, muted, border, accent, good, warn, bad tcell.Style
	focus, marked, key, dialog, input            tcell.Style
	gradient                                     [8]tcell.Style
}

func newTheme(colors int) theme {
	base := tcell.StyleDefault
	t := theme{
		base: base, muted: base.Dim(true), border: base.Dim(true),
		accent: base.Bold(true), good: base.Bold(true), warn: base.Bold(true), bad: base.Bold(true),
		focus: base.Reverse(true).Bold(true), marked: base.Bold(true), key: base.Reverse(true),
		dialog: base, input: base.Underline(true),
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	if colors == 0 || noColor || os.Getenv("CLICOLOR") == "0" || os.Getenv("TERM") == "dumb" {
		for i := range t.gradient {
			t.gradient[i] = base
		}
		return t
	}
	blue, green := tcell.NewRGBColor(56, 189, 248), tcell.NewRGBColor(74, 222, 128)
	if colors < 256 {
		blue, green = tcell.ColorAqua, tcell.ColorGreen
	}
	t.accent = base.Foreground(blue)
	t.good = base.Foreground(green)
	t.warn = base.Foreground(tcell.ColorYellow)
	t.bad = base.Foreground(tcell.ColorRed).Bold(true)
	t.marked = t.good.Bold(true)
	t.key = t.accent.Bold(true)
	if colors >= 256 {
		t.focus = base.Foreground(tcell.NewHexColor(0xf0f9ff)).Background(tcell.NewHexColor(0x16465c)).Bold(true)
		t.dialog = base.Foreground(tcell.NewHexColor(0xe2e8f0)).Background(tcell.NewHexColor(0x1b2938))
		t.input = t.dialog.Background(tcell.NewHexColor(0x101c29))
	}
	// The same blue-to-green ramp as the cp display, in eight shared styles.
	// Tcell can emit runs of equal styles instead of an escape for every cell.
	for i := range t.gradient {
		color := blue
		if colors >= 256 {
			color = tcell.NewRGBColor(56+int32(i)*18/7, 189+int32(i)*33/7, 248-int32(i)*120/7)
		}
		t.gradient[i] = base.Foreground(color)
	}
	return t
}
