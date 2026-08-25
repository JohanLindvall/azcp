package progress

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

const (
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	clearLine  = "\x1b[2K"
	cursorUp   = "\x1b[A"
)

// rateWindow is how far back throughput is averaged. Long enough to be steady,
// short enough to react when a big file finishes.
const rateWindow = 3 * time.Second

// clear erases the live region. The caller must hold r.mu.
func (r *Reporter) clear() {
	if !r.enabled || r.drawn == 0 {
		return
	}
	var b bytes.Buffer
	b.WriteString("\r")
	for i := 0; i < r.drawn; i++ {
		if i > 0 {
			b.WriteString(cursorUp)
		}
		b.WriteString(clearLine)
	}
	r.drawn = 0
	_, _ = r.out.Write(b.Bytes())
}

// render draws the current frame. The caller must hold r.mu.
func (r *Reporter) render() {
	if !r.enabled || r.stopped {
		return
	}
	lines := r.frame()

	var b bytes.Buffer
	b.Grow(len(lines) * (r.width + 16))
	// Erase whatever is there before drawing, so a shorter frame cannot leave
	// remnants of a taller one behind.
	if r.drawn > 0 {
		b.WriteString("\r")
		for i := 0; i < r.drawn; i++ {
			if i > 0 {
				b.WriteString(cursorUp)
			}
			b.WriteString(clearLine)
		}
	}
	for i, l := range lines {
		b.WriteString(clearLine)
		b.WriteString(l)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	r.drawn = len(lines)
	_, _ = r.out.Write(b.Bytes())
}

// frame builds the lines of the live region.
func (r *Reporter) frame() []string {
	width := r.width
	if width < 24 {
		// Too narrow for a layout; a single terse line is better than wrapping.
		return []string{r.pal.dim(fmt.Sprintf("%s %s",
			r.spin(), humanize.Bytes(r.doneBytes.Load())))}
	}

	doneB := r.doneBytes.Load()
	totalB := r.plannedBytes.Load()
	doneF := r.doneFiles.Load()
	totalF := r.plannedFiles.Load()
	rate := r.rate(doneB)

	var lines []string
	lines = append(lines, r.headerLine(width, doneF, totalF, doneB, totalB))
	lines = append(lines, r.barLine(width, doneB, totalB, rate))

	if n := len(r.active); n > 0 {
		lines = append(lines, "")
		shown := min(n, r.maxRows)
		for _, t := range r.active[:shown] {
			lines = append(lines, r.taskLine(width, t))
		}
		if n > shown {
			lines = append(lines, r.pal.dim(fmt.Sprintf("    … and %d more in flight", n-shown)))
		}
	}
	return lines
}

func (r *Reporter) headerLine(width int, doneF, totalF, doneB, totalB int64) string {
	var parts []string
	if totalF > 0 {
		parts = append(parts, fmt.Sprintf("%s/%s files",
			humanize.Count(doneF), humanize.Count(totalF)))
	} else {
		parts = append(parts, fmt.Sprintf("%s files", humanize.Count(doneF)))
	}
	if totalB > 0 {
		parts = append(parts, fmt.Sprintf("%s/%s", humanize.Bytes(doneB), humanize.Bytes(totalB)))
	} else if doneB > 0 {
		parts = append(parts, humanize.Bytes(doneB))
	}
	if f := r.failedFiles.Load(); f > 0 {
		parts = append(parts, r.pal.bad(fmt.Sprintf("%s failed", humanize.Count(f))))
	}
	if s := r.skippedFiles.Load(); s > 0 {
		parts = append(parts, r.pal.warn(fmt.Sprintf("%s skipped", humanize.Count(s))))
	}
	phase := r.phase
	if r.scanning {
		phase += " (scanning)"
	}
	head := fmt.Sprintf("%s %s  %s", r.spin(), r.pal.bold(phase),
		r.pal.dim(strings.Join(parts, r.pal.sep(" · "))))
	return truncateANSI(head, width)
}

func (r *Reporter) barLine(width int, doneB, totalB int64, rate float64) string {
	right := fmt.Sprintf("  %10s", humanize.Rate(rate))
	if totalB > 0 && rate > 0 && doneB < totalB {
		eta := time.Duration(float64(totalB-doneB)/rate) * time.Second
		right += fmt.Sprintf("  eta %-7s", humanize.Duration(eta))
	} else {
		right += strings.Repeat(" ", 12)
	}
	pctW := 5
	barW := width - humanize.Width(right) - pctW - 2
	if barW < 8 {
		return truncateANSI(r.pal.dim(strings.TrimSpace(right)), width)
	}

	if totalB <= 0 {
		// Nothing to measure against yet: sweep a highlight along the bar so
		// it is obvious work is happening.
		return " " + r.indeterminate(barW) + r.pal.dim(right)
	}
	frac := float64(doneB) / float64(totalB)
	frac = min(max(frac, 0), 1)
	return " " + r.gradientBar(barW, frac) +
		r.pal.dim(fmt.Sprintf("%4.0f%%", frac*100)) + r.pal.dim(right)
}

func (r *Reporter) taskLine(width int, t *Task) string {
	glyph := t.dir.glyph()
	if msg := t.retryMsg.Load(); msg != nil {
		name := humanize.Elide(t.name, max(width-humanize.Width(*msg)-8, 8))
		return fmt.Sprintf("  %s %s  %s",
			r.pal.warn("⟳"), name, r.pal.warn(*msg))
	}
	got := t.transferred.Load()

	// Fixed-width right-hand side keeps the bars aligned as names change.
	const barW = 10
	rateStr := ""
	if el := time.Since(t.start).Seconds(); el > 0.4 && got > 0 {
		rateStr = humanize.Rate(float64(got) / el)
	}
	right := fmt.Sprintf(" %s %4s %10s", "", "", "")
	if t.size > 0 {
		frac := min(max(float64(got)/float64(t.size), 0), 1)
		right = fmt.Sprintf(" %s %3.0f%% %10s", r.plainBar(barW, frac), frac*100, rateStr)
	} else {
		right = fmt.Sprintf(" %s %4s %10s", strings.Repeat("·", barW),
			humanize.Bytes(got), rateStr)
	}
	nameW := width - humanize.Width(stripANSI(right)) - 5
	if nameW < 8 {
		nameW = 8
	}
	return fmt.Sprintf("  %s %s%s", r.pal.accent(glyph),
		humanize.Pad(t.name, nameW), r.pal.dim(right))
}

// blocks are the partial-cell glyphs that give the bar sub-character precision.
var blocks = [...]rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}

func barCells(width int, frac float64) (full int, partial rune) {
	total := frac * float64(width)
	full = int(total)
	if full > width {
		full = width
	}
	rem := total - float64(full)
	idx := int(rem * 8)
	if idx < 0 {
		idx = 0
	}
	if idx > 7 {
		idx = 7
	}
	return full, blocks[idx]
}

// gradientBar draws the overall bar, shading it from the start colour to the
// end colour across its length when the terminal can show it.
func (r *Reporter) gradientBar(width int, frac float64) string {
	full, partial := barCells(width, frac)
	var b strings.Builder
	for i := 0; i < width; i++ {
		switch {
		case i < full:
			b.WriteString(r.pal.gradient(float64(i)/float64(width), "█"))
		case i == full && partial != ' ':
			b.WriteString(r.pal.gradient(float64(i)/float64(width), string(partial)))
		default:
			b.WriteString(r.pal.track("░"))
		}
	}
	return b.String()
}

func (r *Reporter) plainBar(width int, frac float64) string {
	full, partial := barCells(width, frac)
	var b strings.Builder
	for i := 0; i < width; i++ {
		switch {
		case i < full:
			b.WriteString(r.pal.accent("█"))
		case i == full && partial != ' ':
			b.WriteString(r.pal.accent(string(partial)))
		default:
			b.WriteString(r.pal.track("░"))
		}
	}
	return b.String()
}

// indeterminate animates a moving highlight for the period before the work list
// is known.
func (r *Reporter) indeterminate(width int) string {
	const runLen = 6
	pos := (r.spinner * 2) % (width + runLen)
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i >= pos-runLen && i < pos {
			b.WriteString(r.pal.gradient(float64(i)/float64(width), "█"))
		} else {
			b.WriteString(r.pal.track("░"))
		}
	}
	return b.String()
}

var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (r *Reporter) spin() string {
	return r.pal.accent(spinnerFrames[r.spinner%len(spinnerFrames)])
}

// rate averages throughput over a short trailing window. The caller must hold
// r.mu.
func (r *Reporter) rate(doneB int64) float64 {
	now := time.Now()
	r.samples = append(r.samples, sample{now, doneB})
	cut := now.Add(-rateWindow)
	drop := 0
	for drop < len(r.samples)-1 && r.samples[drop].at.Before(cut) {
		drop++
	}
	r.samples = r.samples[drop:]
	if len(r.samples) < 2 {
		return 0
	}
	first, last := r.samples[0], r.samples[len(r.samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	if secs <= 0 {
		return 0
	}
	return float64(last.bytes-first.bytes) / secs
}

// Summary writes the closing report. It is printed after the live region is
// gone, so it stays in the scrollback.
func (r *Reporter) Summary(w *os.File, dryRun bool) {
	// With no live display there was no terminal to summarise for, and cp is
	// silent on success; staying quiet keeps scripts that check stderr happy.
	if !r.enabled {
		return
	}
	done, failed, skipped, retries, bytes, elapsed := r.Totals()
	if done == 0 && failed == 0 && skipped == 0 {
		return
	}
	verb := "Copied"
	if dryRun {
		verb = "Would copy"
	}
	rate := ""
	if s := elapsed.Seconds(); s > 0 && bytes > 0 && !dryRun {
		rate = fmt.Sprintf(" (%s)", humanize.Rate(float64(bytes)/s))
	}
	mark := r.pal.good("✔")
	if failed > 0 {
		mark = r.pal.bad("✖")
	}
	fmt.Fprintf(w, "%s %s %s %s · %s in %s%s\n", mark, verb,
		humanize.Count(done), plural(done, "file", "files"),
		humanize.Bytes(bytes), humanize.Duration(elapsed), rate)

	var notes []string
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%s skipped", humanize.Count(skipped)))
	}
	if failed > 0 {
		notes = append(notes, r.pal.bad(fmt.Sprintf("%s failed", humanize.Count(failed))))
	}
	if retries > 0 {
		notes = append(notes, fmt.Sprintf("%s transient %s retried",
			humanize.Count(retries), plural(retries, "error", "errors")))
	}
	if len(notes) > 0 {
		fmt.Fprintf(w, "  %s\n", r.pal.dim(strings.Join(notes, ", ")))
	}
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
