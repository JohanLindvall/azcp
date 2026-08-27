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
	// eraseBelow clears from the cursor to the end of the screen. The live
	// region is the last thing on it, so this takes the whole region however
	// many rows it turns out to occupy — which is the only way to be rid of a
	// frame the terminal has re-wrapped since it was drawn.
	eraseBelow = "\x1b[J"
)

// rateWindow is how far back throughput is averaged. Long enough to be steady,
// short enough to react when a big file finishes.
const rateWindow = 3 * time.Second

// erase writes the sequence that removes the live region, leaving the cursor
// where the region began. The caller must hold r.paint.
//
// Lines within the region are separated by real newlines, so a resize can only
// ever make it taller: a terminal that re-wraps on resize splits a line that no
// longer fits and never rejoins one that was broken deliberately. Moving up
// r.drawn-1 rows therefore lands inside the region rather than above it, and
// erasing to the end of the screen from there takes whatever the re-wrap made
// of the rest.
func (r *Reporter) erase(b *bytes.Buffer) {
	if r.drawn == 0 {
		return
	}
	b.WriteString("\r")
	for i := 1; i < r.drawn; i++ {
		b.WriteString(cursorUp)
	}
	b.WriteString(eraseBelow)
	r.drawn = 0
}

// clear erases the live region. The caller must hold r.paint.
func (r *Reporter) clear() {
	if !r.enabled || r.drawn == 0 {
		return
	}
	var b bytes.Buffer
	r.erase(&b)
	_, _ = r.out.Write(b.Bytes())
}

// render draws the current frame. The caller must hold r.paint.
func (r *Reporter) render() {
	if !r.enabled || r.stopped {
		return
	}
	lines := r.frame()

	var b bytes.Buffer
	b.Grow(len(lines) * (r.width + 16))
	// Erase whatever is there before drawing, so a shorter frame cannot leave
	// remnants of a taller one behind.
	r.erase(&b)
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

// frame builds the lines of the live region. The caller holds r.paint; the
// mutable state is snapshotted under r.mu so that a worker starting or
// finishing a file never waits on the terminal.
func (r *Reporter) frame() []string {
	r.mu.Lock()
	active := append([]*Task(nil), r.active...)
	phase, scanning := r.phase, r.scanning
	r.mu.Unlock()

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
	lines = append(lines, r.headerLine(width, phase, scanning, doneF, totalF, doneB, totalB))
	lines = append(lines, r.barLine(width, doneB, totalB, rate))

	if n := len(active); n > 0 {
		lines = append(lines, "")
		shown := min(n, r.maxRows)
		for _, t := range active[:shown] {
			lines = append(lines, r.taskLine(width, t))
		}
		if n > shown {
			lines = append(lines, r.pal.dim(fmt.Sprintf("    … and %d more in flight", n-shown)))
		}
	}
	return lines
}

func (r *Reporter) headerLine(width int, phase string, scanning bool,
	doneF, totalF, doneB, totalB int64) string {
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
	// Worth saying only when it differs from the work found: on a first copy
	// every file seen becomes a file planned, and repeating it says nothing.
	if seen := r.seenFiles.Load(); seen > totalF {
		parts = append(parts, fmt.Sprintf("%s seen", humanize.Count(seen)))
	}
	if f := r.failedFiles.Load(); f > 0 {
		parts = append(parts, r.pal.bad(fmt.Sprintf("%s failed", humanize.Count(f))))
	}
	if s := r.skippedFiles.Load(); s > 0 {
		parts = append(parts, r.pal.warn(fmt.Sprintf("%s skipped", humanize.Count(s))))
	}
	if scanning {
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

// gradientBands is how many colour steps the shaded bar is quantised into.
// Colouring every cell individually would be smoother, but it costs one escape
// sequence per character — about 1.7 kB for a single 70-cell bar, repainted
// over and over. Eight bands look continuous and cost eight escapes.
const gradientBands = 8

// gradientBar draws the overall bar, shading it from the start colour to the
// end colour across its length when the terminal can show it.
func (r *Reporter) gradientBar(width int, frac float64) string {
	full, partial := barCells(width, frac)
	var b strings.Builder
	filled := func(i int) bool { return i < full || (i == full && partial != ' ') }
	band := func(i int) int { return i * gradientBands / width }

	for i := 0; i < width; {
		if !filled(i) {
			j := i
			for j < width && !filled(j) {
				j++
			}
			b.WriteString(r.pal.track(strings.Repeat("░", j-i)))
			i = j
			continue
		}
		// One run per colour band, so the whole band shares an escape.
		j := i
		var run strings.Builder
		for j < width && filled(j) && band(j) == band(i) {
			if j == full {
				run.WriteRune(partial)
			} else {
				run.WriteRune('█')
			}
			j++
		}
		b.WriteString(r.pal.gradient(float64(i)/float64(width), run.String()))
		i = j
	}
	return b.String()
}

// plainBar is the single-colour bar used for individual transfers: three
// escapes regardless of width.
func (r *Reporter) plainBar(width int, frac float64) string {
	full, partial := barCells(width, frac)
	var b strings.Builder
	if full > 0 {
		b.WriteString(r.pal.accent(strings.Repeat("█", full)))
	}
	rest := width - full
	if partial != ' ' && rest > 0 {
		b.WriteString(r.pal.accent(string(partial)))
		rest--
	}
	if rest > 0 {
		b.WriteString(r.pal.track(strings.Repeat("░", rest)))
	}
	return b.String()
}

// indeterminate animates a moving highlight for the period before the work list
// is known.
func (r *Reporter) indeterminate(width int) string {
	const runLen = 6
	pos := (r.spinner * 2) % (width + runLen)
	lo := max(pos-runLen, 0)
	hi := min(pos, width)
	var b strings.Builder
	if lo > 0 {
		b.WriteString(r.pal.track(strings.Repeat("░", lo)))
	}
	if hi > lo {
		b.WriteString(r.pal.gradient(float64(lo)/float64(width), strings.Repeat("█", hi-lo)))
	}
	if width > hi {
		b.WriteString(r.pal.track(strings.Repeat("░", width-hi)))
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
	if seen := r.seenFiles.Load(); seen > done+skipped {
		notes = append(notes, fmt.Sprintf("%s seen", humanize.Count(seen)))
	}
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
