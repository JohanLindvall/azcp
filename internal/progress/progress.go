// Package progress draws the live transfer display.
//
// It owns the terminal while it is running: log records and ordinary output go
// through Guard, which erases the live region, lets the caller write, and
// redraws. Everything degrades cleanly — no terminal, no colour, or a very
// narrow window each drop features rather than breaking the layout.
package progress

import (
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

// Mode controls whether the display is drawn.
type Mode int

const (
	// ModeAuto draws only when the output is an interactive terminal.
	ModeAuto Mode = iota
	// ModeAlways draws whatever the output is.
	ModeAlways
	// ModeNever draws nothing, and prints no closing summary either.
	ModeNever
)

// Direction picks the glyph shown beside a transfer.
type Direction int

const (
	// DirLocal is a copy between two local paths.
	DirLocal Direction = iota
	// DirUpload is a copy into blob storage.
	DirUpload
	// DirDownload is a copy out of blob storage.
	DirDownload
	// DirRemote is a copy between two blobs.
	DirRemote
)

func (d Direction) glyph() string {
	switch d {
	case DirUpload:
		return "↑"
	case DirDownload:
		return "↓"
	case DirRemote:
		return "⇄"
	default:
		return "→"
	}
}

// Config configures the reporter.
type Config struct {
	Mode Mode
	Out  *os.File
	// MaxRows caps how many in-flight transfers are listed individually.
	MaxRows int
	// Interval is how often the display repaints. Zero means DefaultInterval.
	Interval time.Duration
}

// DefaultInterval is how often the live display repaints. A progress bar is
// read, not watched: once a second conveys everything useful while keeping the
// terminal quiet, which matters over a slow link where every frame is bytes on
// the wire.
const DefaultInterval = time.Second

// Reporter renders progress. The zero value is not usable; call New.
type Reporter struct {
	out     *os.File
	enabled bool
	pal     palette
	maxRows int

	// mu guards the mutable state below. It is held only long enough to read
	// or update it — never across a write to the terminal.
	mu       sync.Mutex
	phase    string
	scanning bool

	// paint guards the screen: the frame currently on it, the width it was
	// laid out for, and the writes themselves. Workers never take it, so a
	// slow or blocked terminal cannot hold up a transfer.
	paint   sync.Mutex
	width   int
	drawn   int // lines currently occupying the live region
	stopped bool
	spinner int
	samples []sample

	// seenFiles counts what the scan has looked at, whether or not it turned
	// into work. On a re-run that skips almost everything it is the only
	// number that moves, and so the only sign the scan is getting anywhere.
	seenFiles    atomic.Int64
	plannedFiles atomic.Int64
	plannedBytes atomic.Int64
	doneFiles    atomic.Int64
	doneBytes    atomic.Int64
	failedFiles  atomic.Int64
	skippedFiles atomic.Int64
	retries      atomic.Int64
	// partUp and partDown count transfers cut off part-way by the run being
	// cancelled — what an interrupted run leaves behind, and the only thing
	// --resume has anything to say about.
	partUp   atomic.Int64
	partDown atomic.Int64

	active   []*Task
	started  time.Time
	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup

	// interval is how often the display repaints.
	interval time.Duration

	// termWidth reports the terminal width. Tests replace it, since a resize
	// cannot be staged on anything that is not a real terminal.
	termWidth func() (int, error)
}

type sample struct {
	at    time.Time
	bytes int64
}

// New creates a reporter. It does not draw anything until Start is called.
func New(cfg Config) *Reporter {
	out := cfg.Out
	if out == nil {
		out = os.Stderr
	}
	rows := cfg.MaxRows
	if rows <= 0 {
		rows = 8
	}
	isTTY := term.IsTerminal(int(out.Fd()))
	enabled := cfg.Mode == ModeAlways || (cfg.Mode == ModeAuto && isTTY)

	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	r := &Reporter{
		out:      out,
		enabled:  enabled,
		pal:      detectPalette(isTTY),
		maxRows:  rows,
		width:    80,
		phase:    "Copying",
		done:     make(chan struct{}),
		started:  time.Now(),
		interval: interval,
	}
	r.termWidth = func() (int, error) {
		w, _, err := term.GetSize(int(out.Fd()))
		return w, err
	}
	if w, err := r.termWidth(); err == nil && w > 0 {
		r.width = w
	}
	return r
}

// Enabled reports whether a live display will be drawn.
func (r *Reporter) Enabled() bool { return r.enabled }

// Start begins the render loop.
func (r *Reporter) Start() {
	if !r.enabled {
		return
	}
	r.write(hideCursor)
	r.wg.Go(func() {
		t := time.NewTicker(r.interval)
		defer t.Stop()
		for {
			select {
			case <-r.done:
				return
			case <-t.C:
				r.paint.Lock()
				r.spinner++
				r.refreshWidth()
				r.render()
				r.paint.Unlock()
			}
		}
	})
}

// Stop erases the live region and restores the cursor. It is safe to call more
// than once, which matters because both the normal path and the signal handler
// call it.
func (r *Reporter) Stop() {
	r.stopOnce.Do(func() {
		if !r.enabled {
			return
		}
		close(r.done)
		r.wg.Wait()
		r.paint.Lock()
		r.clear()
		r.stopped = true
		r.paint.Unlock()
		r.write(showCursor)
	})
}

// Guard runs fn with the live region erased, then redraws. Every write to the
// terminal from elsewhere in the program goes through here.
func (r *Reporter) Guard(fn func()) {
	r.paint.Lock()
	defer r.paint.Unlock()
	r.clear()
	fn()
	r.render()
}

// SetPhase changes the verb shown in the header.
func (r *Reporter) SetPhase(s string) {
	r.mu.Lock()
	r.phase = s
	r.mu.Unlock()
}

// SetScanning marks that the work list is still being built, so totals are
// provisional and the overall bar cannot be trusted yet.
func (r *Reporter) SetScanning(v bool) {
	r.mu.Lock()
	r.scanning = v
	r.mu.Unlock()
}

// Plan adds to the expected totals.
func (r *Reporter) Plan(files, bytes int64) {
	r.plannedFiles.Add(files)
	r.plannedBytes.Add(bytes)
}

// Saw records a file the scan has considered, copied or not.
func (r *Reporter) Saw(n int64) { r.seenFiles.Add(n) }

// Seen reports how many files the scan has considered.
func (r *Reporter) Seen() int64 { return r.seenFiles.Load() }

// Skipped records a file that was deliberately not copied.
func (r *Reporter) Skipped(n int64) { r.skippedFiles.Add(n) }

// Failed records a problem that did not arise from a transfer in flight — a
// source that could not be read, a destination that could not be made — so the
// closing summary accounts for it alongside the transfers that failed.
func (r *Reporter) Failed(n int64) { r.failedFiles.Add(n) }

// Totals reports the current tallies for the closing summary.
func (r *Reporter) Totals() (done, failed, skipped, retries int64, bytes int64, elapsed time.Duration) {
	return r.doneFiles.Load(), r.failedFiles.Load(), r.skippedFiles.Load(),
		r.retries.Load(), r.doneBytes.Load(), time.Since(r.started)
}

// Unfinished reports how many uploads and downloads stopped with bytes already
// moved. A local copy is not counted: there is nothing on the far side holding
// what it managed, so there is nothing to continue into.
func (r *Reporter) Unfinished() (uploads, downloads int64) {
	return r.partUp.Load(), r.partDown.Load()
}

// Task tracks one file in flight.
type Task struct {
	r    *Reporter
	name string
	dir  Direction
	size int64

	transferred atomic.Int64
	// counted is what has already been folded into the reporter's running
	// total, so that an absolute update from the SDK can be turned into the
	// delta the aggregate needs.
	counted  atomic.Int64
	retryMsg atomic.Pointer[string]
	start    time.Time
}

// Begin registers a transfer and returns its handle.
func (r *Reporter) Begin(name string, size int64, dir Direction) *Task {
	t := &Task{r: r, name: name, dir: dir, size: size, start: time.Now()}
	r.mu.Lock()
	r.active = append(r.active, t)
	r.mu.Unlock()
	return t
}

// Set records the absolute number of bytes transferred so far. The Azure SDK
// reports progress this way, and may report a smaller figure after retrying a
// block, so the aggregate is adjusted by the difference rather than assuming
// forward movement.
func (t *Task) Set(n int64) {
	if t == nil {
		return
	}
	t.transferred.Store(n)
	prev := t.counted.Swap(n)
	t.r.doneBytes.Add(n - prev)
}

// Add records additional bytes transferred.
func (t *Task) Add(n int64) {
	if t == nil || n == 0 {
		return
	}
	t.transferred.Add(n)
	t.counted.Add(n)
	t.r.doneBytes.Add(n)
}

// Retrying notes that the transfer hit a transient failure and will be tried
// again, which the display shows in place of the bar.
func (t *Task) Retrying(attempt, maxAttempts int, delay time.Duration) {
	if t == nil {
		return
	}
	t.r.retries.Add(1)
	msg := fmt.Sprintf("retry %d/%d in %s", attempt, maxAttempts, humanize.Duration(delay))
	t.retryMsg.Store(&msg)
}

// Resumed clears a retry notice.
func (t *Task) Resumed() {
	if t == nil {
		return
	}
	t.retryMsg.Store(nil)
}

// Done removes the transfer from the display and folds it into the totals.
func (t *Task) Done(err error) {
	if t == nil {
		return
	}
	r := t.r
	if err != nil {
		r.failedFiles.Add(1)
		// Bytes from a failed transfer are not progress; take them back out so
		// the aggregate keeps matching what actually landed.
		if n := t.counted.Swap(0); n != 0 {
			r.doneBytes.Add(-n)
		}
	} else {
		r.doneFiles.Add(1)
		// Trust the declared size at completion: byte callbacks can lag or be
		// skipped entirely on a server-side copy.
		if n := t.counted.Swap(t.size); n != t.size {
			r.doneBytes.Add(t.size - n)
		}
	}
	r.remove(t)
}

// Interrupted removes a transfer the run was cancelled out from under. It is
// not a failure — nobody needs telling that what they stopped stopped — and it
// is not progress either, so the bytes come back out of the total; what it is
// is unfinished, which is the one thing worth remembering about it.
func (t *Task) Interrupted() {
	if t == nil {
		return
	}
	r := t.r
	// Only a transfer that had moved something is unfinished; one that never
	// started is simply not done, and resuming it would save nothing.
	if n := t.counted.Swap(0); n != 0 {
		r.doneBytes.Add(-n)
		switch t.dir {
		case DirUpload:
			r.partUp.Add(1)
		case DirDownload:
			r.partDown.Add(1)
		}
	}
	r.remove(t)
}

// remove takes a task out of the live display.
func (r *Reporter) remove(t *Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i := slices.Index(r.active, t); i >= 0 {
		r.active = slices.Delete(r.active, i, i+1)
	}
}

func (r *Reporter) write(s string) {
	if !r.enabled {
		return
	}
	_, _ = r.out.WriteString(s)
}

// refreshWidth re-reads the terminal width. It is polled on every frame rather
// than driven by SIGWINCH, so resize handling is identical on platforms that
// have no such signal, at the cost of one cheap query per frame.
//
// The caller must hold r.paint.
func (r *Reporter) refreshWidth() {
	w, err := r.termWidth()
	if err != nil || w <= 0 || w == r.width {
		return
	}
	// Only the width changes here. The frame on screen was laid out for the
	// old one and the terminal may have re-wrapped it since, but it must still
	// be erased rather than abandoned: forgetting it leaves a copy of the whole
	// display in the scrollback every time the window is resized, which is what
	// the erase in render is written to survive.
	r.width = w
}
