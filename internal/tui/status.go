// SPDX-License-Identifier: MIT

package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/progress"
)

func (a *app) drawStatus(w, h int) {
	if a.busy == nil {
		style, mark := a.theme.good, "✓"
		if strings.HasPrefix(a.status, "Error:") {
			style, mark = a.theme.bad, "!"
		} else if strings.HasPrefix(a.status, "Cancelled") {
			style, mark = a.theme.warn, "!"
		}
		a.text(1, h-5, 1, mark, style)
		a.text(3, h-5, w-4, humanize.Elide(safeText(a.status), w-4), style)
		a.text(3, h-4, w-4, "Enter: open · ←: parent · Space: mark · *: invert", a.theme.muted)
		return
	}
	s := a.busy.progress.Snapshot()
	if a.busy.sizing {
		a.text(1, h-5, w-2, humanize.Elide(a.busy.label, w-2), a.theme.accent)
		detail := fmt.Sprintf("%s files · %s so far · Esc: cancel", humanize.Count(s.PlannedFiles), humanize.Bytes(s.PlannedBytes))
		a.text(1, h-4, w-2, humanize.Elide(detail, w-2), a.theme.muted)
		return
	}
	if s.Current != "" {
		detail := fmt.Sprintf("%d active · %s", s.Active, s.Current)
		a.text(1, h-6, w-2, humanize.Elide(safeText(detail), w-2), a.theme.muted)
	}
	label := a.busy.label
	if s.Scanning {
		label += " · scanning"
	}
	if s.PlannedFiles > 0 {
		label += fmt.Sprintf(" · %s / %s files", humanize.Count(s.Done), humanize.Count(s.PlannedFiles))
	}
	if w >= 90 && s.PlannedBytes > 0 {
		label += " · " + humanize.Bytes(s.Bytes) + " / " + humanize.Bytes(s.PlannedBytes)
	}
	right, style := humanize.Duration(s.Elapsed)+" · Esc: cancel", a.theme.muted
	var issues []string
	if s.Failed > 0 {
		issues = append(issues, fmt.Sprintf("%s failed", humanize.Count(s.Failed)))
		style = a.theme.bad
	}
	if s.Retries > 0 {
		issues = append(issues, fmt.Sprintf("%s retried", humanize.Count(s.Retries)))
		if s.Failed == 0 {
			style = a.theme.warn
		}
	}
	if s.Skipped > 0 {
		issues = append(issues, fmt.Sprintf("%s skipped", humanize.Count(s.Skipped)))
	}
	if len(issues) > 0 {
		right = strings.Join(issues, " · ")
	}
	rightWidth := min(w-10, humanize.Width(right))
	spinners := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	spin := spinners[(s.Elapsed/progress.DefaultInterval)%time.Duration(len(spinners))]
	a.text(1, h-5, 1, string(spin), a.theme.accent)
	a.text(3, h-5, w-rightWidth-6, humanize.Elide(safeText(label), w-rightWidth-6), a.theme.base.Bold(true))
	a.right(w-rightWidth-1, h-5, rightWidth, right, style)
	a.drawMeter(3, h-4, w-4, s)
}

func (a *app) drawMeter(x, y, width int, s progress.Snapshot) {
	done, total := s.Bytes, s.PlannedBytes
	if total == 0 {
		done, total = s.Done, s.PlannedFiles
	}
	known := !s.Scanning && total > 0
	fraction, percent := float64(0), "   —"
	if known {
		fraction = min(max(float64(done)/float64(total), 0), 1)
		p := fraction * 100
		if done < total {
			p = min(p, 99)
		}
		percent = fmt.Sprintf("%3.0f%%", p)
	}
	rate := float64(0)
	if s.Elapsed >= 400*time.Millisecond {
		rate = max(0, float64(s.Bytes)/s.Elapsed.Seconds())
	}
	right := "  " + percent + "  " + humanize.Rate(rate)
	if width >= 76 {
		eta := "—"
		if known && s.PlannedBytes > s.Bytes && rate > 0 {
			secs := float64(s.PlannedBytes-s.Bytes) / rate
			if secs < float64((1<<63-1)/int64(time.Second)) {
				eta = humanize.Duration(time.Duration(secs * float64(time.Second)))
			}
		}
		right += "  ETA " + eta
	}
	barWidth := max(1, width-humanize.Width(right))
	a.text(x, y, barWidth, strings.Repeat("─", barWidth), a.theme.border)
	if known {
		filled := fraction * float64(barWidth)
		full := int(filled)
		for i := 0; i < full; i++ {
			a.screen.SetContent(x+i, y, '█', nil, a.theme.gradient[i*len(a.theme.gradient)/barWidth])
		}
		partials := []rune(" ▏▎▍▌▋▊▉")
		part := int((filled - float64(full)) * 8)
		if full < barWidth && part > 0 {
			a.screen.SetContent(x+full, y, partials[part], nil, a.theme.gradient[full*len(a.theme.gradient)/barWidth])
		}
	} else {
		pos := int(s.Elapsed/(100*time.Millisecond)) % (barWidth + 6)
		for i := max(0, pos-6); i < min(pos, barWidth); i++ {
			a.screen.SetContent(x+i, y, '━', nil, a.theme.gradient[i*len(a.theme.gradient)/barWidth])
		}
	}
	a.text(x+barWidth, y, width-barWidth, right, a.theme.accent)
}
