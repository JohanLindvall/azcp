// SPDX-License-Identifier: MIT

package progress

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

func newQuietReporter() *Reporter {
	return New(Config{Mode: ModeNever})
}

// Set is absolute — the Azure SDK can report fewer bytes after retrying a
// block — while Add is a delta for local copies. The aggregate has to stay
// truthful under both, and under the corrections Done and Interrupted apply.
func TestTaskByteAccounting(t *testing.T) {
	r := newQuietReporter()

	up := r.Begin("up", 100, DirUpload)
	up.Set(60)
	up.Set(40) // a retried block reported less; the total must follow it down
	if got := r.doneBytes.Load(); got != 40 {
		t.Fatalf("after Set(60), Set(40): total %d, want 40", got)
	}
	up.Done(nil) // completion trues the task up to its size
	if got := r.doneBytes.Load(); got != 100 {
		t.Fatalf("after Done(nil): total %d, want 100", got)
	}

	lc := r.Begin("local", 50, DirLocal)
	lc.Add(20)
	lc.Add(10)
	if got := r.doneBytes.Load(); got != 130 {
		t.Fatalf("after Add deltas: total %d, want 130", got)
	}
	lc.Done(errors.New("boom")) // a failure's bytes are not progress
	if got := r.doneBytes.Load(); got != 100 {
		t.Fatalf("after failed Done: total %d, want 100", got)
	}

	done, failed, _, _, _, _ := r.Totals()
	if done != 1 || failed != 1 {
		t.Fatalf("done %d failed %d, want 1 and 1", done, failed)
	}
}

func TestActualSizeCorrectsBothProgressTotals(t *testing.T) {
	for _, planned := range []int64{0, 50, 200} {
		r := newQuietReporter()
		r.Plan(1, planned)
		tk := r.Begin("changing.txt", planned, DirLocal)
		tk.Add(100)
		tk.SetSize(100)
		tk.Done(nil)
		_, _, _, _, bytes, _ := r.Totals()
		if total := r.plannedBytes.Load(); bytes != 100 || total != 100 {
			t.Errorf("planned %d: copied %d of %d bytes, want 100 of 100", planned, bytes, total)
		}
	}
}

// An interrupted transfer is neither done nor failed; its bytes come back out
// and it is remembered only as unfinished, per direction.
func TestInterruptedTakesBytesBackAndCountsUnfinished(t *testing.T) {
	r := newQuietReporter()

	dl := r.Begin("dl", 100, DirDownload)
	dl.Set(70)
	dl.Interrupted()

	up := r.Begin("up", 100, DirUpload)
	up.Set(10)
	up.Interrupted()

	idle := r.Begin("idle", 100, DirUpload)
	idle.Interrupted() // never moved a byte: not worth remembering

	if got := r.doneBytes.Load(); got != 0 {
		t.Fatalf("interrupted bytes stayed in the total: %d", got)
	}
	ups, downs := r.Unfinished()
	if ups != 1 || downs != 1 {
		t.Fatalf("unfinished %d up %d down, want 1 and 1", ups, downs)
	}
	done, failed, _, _, _, _ := r.Totals()
	if done != 0 || failed != 0 {
		t.Fatalf("interruption was counted as done=%d failed=%d", done, failed)
	}
}

func TestElapsedTimeStopsWithTheTransfer(t *testing.T) {
	r := newQuietReporter()
	r.Stop()
	_, _, _, _, _, elapsed := r.Totals()
	// Changing the start models time passing without making this a timed test.
	r.started = r.started.Add(-time.Hour)
	_, _, _, _, _, later := r.Totals()
	if elapsed <= 0 || later != elapsed {
		t.Fatalf("elapsed time changed after Stop: %s -> %s", elapsed, later)
	}
}

func TestWorkersCanFinishWhileTheTerminalIsBlocked(t *testing.T) {
	r := newQuietReporter()
	r.Plan(20, 0)
	r.paint.Lock()
	defer r.paint.Unlock()
	done := make(chan struct{})
	go func() {
		var workers sync.WaitGroup
		for range 20 {
			workers.Go(func() {
				task := r.Begin("file", 0, DirLocal)
				task.Add(100)
				task.SetSize(100)
				task.Done(nil)
			})
		}
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		files, failed, _, _, bytes, _ := r.Totals()
		if files != 20 || failed != 0 || bytes != 2000 || r.plannedBytes.Load() != bytes {
			t.Fatalf("concurrent totals: %d files, %d failures, %d bytes", files, failed, bytes)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("terminal painting held up the workers")
	}
}

// The header and bar must fit the width they are given, whatever state the
// run is in — scanning, mid-run with an eta, or rateless.
func TestFrameLinesFitTheWidth(t *testing.T) {
	r := newQuietReporter()
	r.enabled = true
	r.Plan(10, 1000)
	r.Saw(12)
	r.Failed(1)
	r.Skipped(2)
	tk := r.Begin("a/rather/long/path/日本語/👩🏽‍💻/e\u0301/needs-eliding.txt", 500, DirUpload)
	tk.Set(250)
	defer tk.Done(nil)
	rt := r.Begin("retrying.bin", 100, DirDownload)
	rt.Retrying(2, 3, 100*24*time.Hour)
	defer rt.Done(nil)
	r.Begin("empty.txt", 0, DirLocal)
	r.Begin("another.txt", 10, DirRemote)
	r.Begin("hidden.txt", 10, DirRemote)
	r.maxRows = 4

	for _, level := range []colourLevel{levelNone, level16, level256, levelTrue} {
		r.pal = palette{level}
		for width := 1; width <= 120; width++ {
			r.width = width
			for _, scanning := range []bool{true, false} {
				r.SetScanning(scanning)
				for _, l := range r.frame() {
					if got := humanize.Width(stripANSI(l)); got >= r.width {
						t.Fatalf("level %d, scanning %v: line %d cells wide in a %d-cell terminal: %q",
							level, scanning, got, r.width, stripANSI(l))
					}
				}
			}
		}
	}
}
