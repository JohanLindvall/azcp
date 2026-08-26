package progress

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// newTestReporter draws into a file, which is not a terminal — hence
// ModeAlways — and which can be read back to see exactly what was written.
func newTestReporter(t *testing.T) (*Reporter, *os.File) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "frame")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	r := New(Config{Mode: ModeAlways, Out: f, MaxRows: 4})
	if !r.Enabled() {
		t.Fatal("the display is not enabled")
	}
	r.plannedFiles.Store(10)
	r.plannedBytes.Store(1 << 20)
	r.doneFiles.Store(3)
	r.doneBytes.Store(1 << 18)
	return r, f
}

// written returns everything the reporter has written since offset at, along
// with the new offset.
func written(t *testing.T, f *os.File, at int64) (string, int64) {
	t.Helper()
	end, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, end-at)
	if _, err := f.ReadAt(b, at); err != nil {
		t.Fatal(err)
	}
	return string(b), end
}

// Resizing must not leave the old frame behind. The region is erased from
// where it began to the end of the screen, which takes it whatever the
// terminal has since made of it; abandoning it instead — which is what
// forgetting the line count amounts to — puts a copy of the whole display in
// the scrollback every time the window changes size.
func TestResizeErasesTheFrameAlreadyDrawn(t *testing.T) {
	r, f := newTestReporter(t)

	r.paint.Lock()
	r.width = 120
	r.render()
	drawn := r.drawn
	r.paint.Unlock()
	if drawn < 2 {
		t.Fatalf("frame is %d lines, want at least 2", drawn)
	}
	_, at := written(t, f, 0)

	// The window narrows, exactly as the render loop sees it.
	r.termWidth = func() (int, error) { return 40, nil }
	r.paint.Lock()
	r.refreshWidth()
	r.render()
	r.paint.Unlock()
	if r.width != 40 {
		t.Fatalf("width = %d, want the resize to have been picked up", r.width)
	}

	got, _ := written(t, f, at)
	want := "\r" + strings.Repeat(cursorUp, drawn-1) + eraseBelow
	if !strings.HasPrefix(got, want) {
		t.Errorf("the frame after a resize begins %q; it must first erase the %d "+
			"lines already on screen", elide(got, 32), drawn)
	}
}

// Every frame erases the one before it, whatever their relative heights: a
// taller frame must not be left showing beneath a shorter one.
func TestEachFrameErasesTheLast(t *testing.T) {
	r, f := newTestReporter(t)

	// Several files in flight makes a tall frame; none makes a short one.
	for range 3 {
		r.active = append(r.active, &Task{r: r, name: "some/blob", size: 100})
	}
	r.paint.Lock()
	r.render()
	tall := r.drawn
	r.paint.Unlock()
	_, at := written(t, f, 0)

	r.active = nil
	r.paint.Lock()
	r.render()
	short := r.drawn
	r.paint.Unlock()
	if short >= tall {
		t.Fatalf("frames are %d and %d lines; the second should be shorter", tall, short)
	}

	got, _ := written(t, f, at)
	want := "\r" + strings.Repeat(cursorUp, tall-1) + eraseBelow
	if !strings.HasPrefix(got, want) {
		t.Errorf("a shorter frame begins %q; it must erase all %d lines of the "+
			"taller one first", elide(got, 32), tall)
	}
}

func elide(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// An interrupted transfer is not a failed one. Counting it as failed is what
// turns "I pressed Ctrl-C" into a summary claiming dozens of things went wrong,
// and it loses the one fact worth keeping: that part of it arrived.
func TestInterruptedIsNotFailed(t *testing.T) {
	r := New(Config{Mode: ModeNever})

	tk := r.Begin("some/blob", 1000, DirDownload)
	tk.Set(400)
	tk.Interrupted()

	done, failed, skipped, _, bytes, _ := r.Totals()
	if failed != 0 {
		t.Errorf("failed = %d, want 0: stopping a run is not a failure", failed)
	}
	if done != 0 {
		t.Errorf("copied = %d, want 0: the file never arrived whole", done)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if bytes != 0 {
		t.Errorf("bytes = %d, want 0: what arrived is not a copied file", bytes)
	}
	if up, down := r.Unfinished(); up != 0 || down != 1 {
		t.Errorf("unfinished = %d up, %d down, want 0 and 1", up, down)
	}

	// One that never moved a byte has nothing to resume into.
	r.Begin("another/blob", 1000, DirUpload).Interrupted()
	if up, _ := r.Unfinished(); up != 0 {
		t.Errorf("unfinished uploads = %d, want 0 for a transfer that never started", up)
	}

	// And a real failure still counts as one.
	f := r.Begin("bad/blob", 10, DirDownload)
	f.Done(errors.New("no such blob"))
	if _, failed, _, _, _, _ := r.Totals(); failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}
