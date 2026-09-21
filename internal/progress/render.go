package progress

import (
	"bytes"
	"fmt"
	"io"
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

	// Leave the last cell alone so terminals with automatic margins do not
	// wrap a full row and change the height of the live region.
	width := max(r.width-1, 0)
	doneB := r.doneBytes.Load()
	totalB := r.plannedBytes.Load()
	doneF := r.doneFiles.Load()
	totalF := r.plannedFiles.Load()
	rate := r.rate(doneB)
	if width < 32 {
		label := phase
		if scanning {
			label = "Scanning"
		}
		return []string{truncateANSI(fmt.Sprintf(" %s %s · %s",
			r.spin(), r.pal.bold(label), humanize.Bytes(doneB)), width)}
	}

	lines := []string{r.headerLine(width, phase, scanning)}
	files := r.pal.bold(humanize.Count(doneF))
	if totalF > 0 {
		files += r.pal.dim(" / " + humanize.Count(totalF))
	}
	files += r.pal.dim(" files")
	bytes := r.pal.bold(humanize.Bytes(doneB))
	if totalB > 0 {
		bytes += r.pal.dim(" / " + humanize.Bytes(totalB))
	}
	lines = append(lines, r.detailLines(width, []string{files, bytes})...)
	if totalB > 0 {
		lines = append(lines, r.barLine(width, doneB, totalB, rate, scanning))
	} else {
		// Empty files still make progress, but a byte rate cannot estimate
		// how long creating them will take.
		lines = append(lines, r.barLine(width, doneF, totalF, 0, scanning))
	}
	lines = append(lines, r.statusLines(width, totalF)...)

	if n := len(active); n > 0 {
		lines = append(lines, "", r.taskHeading(width, n))
		shown := min(n, r.maxRows)
		for _, t := range active[:shown] {
			lines = append(lines, r.taskLine(width, t))
		}
		if n > shown {
			lines = append(lines, truncateANSI(r.pal.dim(fmt.Sprintf("   … %d more active", n-shown)), width))
		}
	}
	return lines
}

func (r *Reporter) headerLine(width int, phase string, scanning bool) string {
	head := " " + r.spin() + " " + r.pal.bold(phase)
	if scanning {
		head += r.pal.dim(" · scanning")
	}
	return joinSides(head, r.pal.dim(humanize.Duration(time.Since(r.started))+" elapsed"), width)
}

// Keep the exceptional counts separate from the totals: clipping a long
// header used to hide failures and retries just when they mattered most.
func (r *Reporter) statusLines(width int, totalF int64) []string {
	var parts []string
	if f := r.failedFiles.Load(); f > 0 {
		parts = append(parts, r.pal.bad(fmt.Sprintf("%s failed", humanize.Count(f))))
	}
	if n := r.retries.Load(); n > 0 {
		parts = append(parts, r.pal.warn(fmt.Sprintf("%s retried", humanize.Count(n))))
	}
	if s := r.skippedFiles.Load(); s > 0 {
		parts = append(parts, r.pal.dim(fmt.Sprintf("%s skipped", humanize.Count(s))))
	}
	if seen := r.seenFiles.Load(); seen > totalF {
		parts = append(parts, r.pal.dim(fmt.Sprintf("%s seen", humanize.Count(seen))))
	}
	return r.detailLines(width, parts)
}

func joinSides(left, right string, width int) string {
	gap := width - humanize.Width(stripANSI(left)) - humanize.Width(stripANSI(right))
	if gap >= 2 {
		return left + strings.Repeat(" ", gap) + right
	}
	return truncateANSI(left, width)
}

func (r *Reporter) detailLines(width int, parts []string) []string {
	var lines []string
	line := ""
	for _, part := range parts {
		if line != "" && humanize.Width(stripANSI(line))+3+humanize.Width(stripANSI(part)) > width {
			lines = append(lines, line)
			line = ""
		}
		if line == "" {
			line = "   " + truncateANSI(part, width-3)
		} else {
			line += r.pal.dim(" · ") + part
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func (r *Reporter) barLine(width int, done, total int64, rate float64, scanning bool) string {
	determinate := total > 0 && !scanning
	frac := float64(0)
	percent := r.pal.dim("   —")
	if determinate {
		frac = min(max(float64(done)/float64(total), 0), 1)
		percent = r.pal.bold(fmt.Sprintf("%3.0f%%", frac*100))
	}
	right := "  " + percent
	if width >= 44 {
		right += "  " + r.pal.accent(fmt.Sprintf("%10s", humanize.Rate(rate)))
	}
	if width >= 64 {
		eta := "—"
		if determinate && rate > 0 && done < total {
			secs := float64(total-done) / rate
			if secs < float64((1<<63-1)/int64(time.Second)) {
				eta = humanize.Duration(time.Duration(secs) * time.Second)
			}
		}
		right += r.pal.dim(fmt.Sprintf("  ETA %-7s", eta))
	}
	barW := width - humanize.Width(stripANSI(right)) - 3
	bar := r.indeterminate(barW)
	if determinate {
		bar = r.gradientBar(barW, frac)
	}
	return "   " + bar + right
}

const taskBarWidth = 10

// Dropping columns buys space for the filename before eliding it. The same
// layout is used for the column headings and every transfer in the frame.
func taskColumns(width int, bar, percent, rate string) string {
	right := "  "
	if width >= 72 {
		right += bar + " "
	}
	right += percent
	if width >= 52 {
		right += "  " + rate
	}
	return right
}

func (r *Reporter) taskHeading(width, active int) string {
	label := fmt.Sprintf("   Transfers · %d active", active)
	if width < 52 {
		label = fmt.Sprintf("   Transfers · %d", active)
	}
	right := taskColumns(width, strings.Repeat(" ", taskBarWidth), "Done", fmt.Sprintf("%10s", "Rate"))
	return r.pal.dim(joinSides(label, right, width))
}

func (r *Reporter) taskLine(width int, t *Task) string {
	if msg := t.retryMsg.Load(); msg != nil {
		nameW := width - humanize.Width(*msg) - 5
		prefix := " " + r.pal.warn("⟳") + " "
		if nameW < 8 {
			return truncateANSI(prefix+r.pal.warn(*msg), width)
		}
		return prefix + r.pal.filename(t.name, nameW) + "  " + r.pal.warn(*msg)
	}
	got := t.transferred.Load()

	rateStr := "—"
	if el := time.Since(t.start).Seconds(); el > 0.4 && got > 0 {
		rateStr = humanize.Rate(float64(got) / el)
	}
	bar, percent := r.indeterminate(taskBarWidth), "   —"
	if t.size > 0 {
		frac := min(max(float64(got)/float64(t.size), 0), 1)
		bar, percent = r.plainBar(taskBarWidth, frac), fmt.Sprintf("%3.0f%%", frac*100)
	}
	right := taskColumns(width, bar, r.pal.dim(percent), r.pal.dim(fmt.Sprintf("%10s", rateStr)))
	nameW := width - humanize.Width(stripANSI(right)) - 3
	return " " + r.pal.accent(t.dir.glyph()) + " " + r.pal.filename(t.name, nameW) + right
}

// blocks are the partial-cell glyphs that give the bar sub-character precision.
var blocks = [...]rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}

func barCells(width int, frac float64) (full int, partial rune) {
	total := frac * float64(width)
	full = min(int(total), width)
	idx := min(max(int((total-float64(full))*8), 0), len(blocks)-1)
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
			b.WriteString(r.pal.track(strings.Repeat("─", j-i)))
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
		b.WriteString(r.pal.track(strings.Repeat("─", rest)))
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
		b.WriteString(r.pal.track(strings.Repeat("─", lo)))
	}
	if hi > lo {
		b.WriteString(r.pal.gradient(float64(lo)/float64(width), strings.Repeat("█", hi-lo)))
	}
	if width > hi {
		b.WriteString(r.pal.track(strings.Repeat("─", width-hi)))
	}
	return b.String()
}

var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (r *Reporter) spin() string {
	return r.pal.accent(spinnerFrames[r.spinner%len(spinnerFrames)])
}

// rate averages throughput over a short trailing window. The samples belong to
// the paint lock, which the caller must hold.
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
func (r *Reporter) Summary(w io.Writer, dryRun bool) {
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
	result := fmt.Sprintf("%s %s %s", verb, humanize.Count(done), humanize.Plural(done, "file", "files"))
	fmt.Fprintf(w, " %s %s%s%s%s\n", mark, r.pal.bold(result), r.pal.dim(" · "),
		r.pal.bold(humanize.Bytes(bytes)), r.pal.dim(" in "+humanize.Duration(elapsed)+rate))

	var notes []string
	if seen := r.seenFiles.Load(); seen > done+skipped {
		notes = append(notes, r.pal.dim(fmt.Sprintf("%s seen", humanize.Count(seen))))
	}
	if skipped > 0 {
		notes = append(notes, r.pal.dim(fmt.Sprintf("%s skipped", humanize.Count(skipped))))
	}
	if failed > 0 {
		notes = append(notes, r.pal.bad(fmt.Sprintf("%s failed", humanize.Count(failed))))
	}
	if retries > 0 {
		notes = append(notes, r.pal.warn(fmt.Sprintf("%s transient %s retried",
			humanize.Count(retries), humanize.Plural(retries, "error", "errors"))))
	}
	if len(notes) > 0 {
		fmt.Fprintf(w, "   %s\n", strings.Join(notes, r.pal.dim(" · ")))
	}
}
