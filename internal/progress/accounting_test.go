package progress

import (
	"errors"
	"testing"
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

// The header and bar must fit the width they are given, whatever state the
// run is in — scanning, mid-run with an eta, or rateless.
func TestFrameLinesFitTheWidth(t *testing.T) {
	r := newQuietReporter()
	r.enabled = true
	r.width = 60
	r.Plan(10, 1000)
	r.Saw(12)
	r.Failed(1)
	r.Skipped(2)
	tk := r.Begin("a/rather/long/path/that/will/need/eliding.txt", 500, DirUpload)
	tk.Set(250)
	defer tk.Done(nil)
	rt := r.Begin("retrying.bin", 100, DirDownload)
	rt.Retrying(2, 3, 0)
	defer rt.Done(nil)

	for _, phase := range []bool{true, false} {
		r.SetScanning(phase)
		for _, l := range r.frame() {
			if got := len([]rune(stripANSI(l))); got > r.width {
				t.Errorf("line %d cells wide in a %d-cell terminal: %q",
					got, r.width, stripANSI(l))
			}
		}
	}
}
