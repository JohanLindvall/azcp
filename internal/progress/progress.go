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
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

// Mode controls whether the display is drawn.
type Mode int

const (
	// ModeAuto draws only when the output is an interactive terminal.
	ModeAuto Mode = iota
	ModeAlways
	ModeNever
)

// ParseMode maps a --progress value.
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return ModeAuto, nil
	case "always", "yes", "force":
		return ModeAlways, nil
	case "never", "no", "none":
		return ModeNever, nil
	}
	return 0, fmt.Errorf("unknown progress mode %q (want auto, always or never)", s)
}

// Direction picks the glyph shown beside a transfer.
type Direction int

const (
	DirLocal Direction = iota
	DirUpload
	DirDownload
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
}

// Reporter renders progress. The zero value is not usable; call New.
type Reporter struct {
	out     *os.File
	enabled bool
	pal     palette
	maxRows int

	mu      sync.Mutex
	width   int
	drawn   int // lines currently occupying the live region
	stopped bool

	phase    string
	scanning bool

	plannedFiles atomic.Int64
	plannedBytes atomic.Int64
	doneFiles    atomic.Int64
	doneBytes    atomic.Int64
	failedFiles  atomic.Int64
	skippedFiles atomic.Int64
	retries      atomic.Int64

	active   []*Task
	spinner  int
	samples  []sample
	started  time.Time
	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
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

	r := &Reporter{
		out:     out,
		enabled: enabled,
		pal:     detectPalette(isTTY),
		maxRows: rows,
		width:   80,
		phase:   "Copying",
		done:    make(chan struct{}),
		started: time.Now(),
	}
	if w, _, err := term.GetSize(int(out.Fd())); err == nil && w > 0 {
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
	r.watchResize()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(80 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-r.done:
				return
			case <-t.C:
				r.mu.Lock()
				r.spinner++
				r.render()
				r.mu.Unlock()
			}
		}
	}()
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
		r.mu.Lock()
		r.clear()
		r.stopped = true
		r.mu.Unlock()
		r.write(showCursor)
	})
}

// Guard runs fn with the live region erased, then redraws. Every write to the
// terminal from elsewhere in the program goes through here.
func (r *Reporter) Guard(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

// Skipped records a file that was deliberately not copied.
func (r *Reporter) Skipped(n int64) { r.skippedFiles.Add(n) }

// Totals reports the current tallies for the closing summary.
func (r *Reporter) Totals() (done, failed, skipped, retries int64, bytes int64, elapsed time.Duration) {
	return r.doneFiles.Load(), r.failedFiles.Load(), r.skippedFiles.Load(),
		r.retries.Load(), r.doneBytes.Load(), time.Since(r.started)
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
	r.mu.Lock()
	for i, a := range r.active {
		if a == t {
			r.active = append(r.active[:i], r.active[i+1:]...)
			break
		}
	}
	r.mu.Unlock()
}

func (r *Reporter) write(s string) {
	if !r.enabled {
		return
	}
	_, _ = r.out.WriteString(s)
}

func (r *Reporter) watchResize() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer signal.Stop(ch)
		for {
			select {
			case <-r.done:
				return
			case <-ch:
				if w, _, err := term.GetSize(int(r.out.Fd())); err == nil && w > 0 {
					r.mu.Lock()
					// The old frame was laid out for the old width; forget it
					// rather than trying to erase lines that have reflowed.
					r.width = w
					r.drawn = 0
					r.mu.Unlock()
				}
			}
		}
	}()
}
