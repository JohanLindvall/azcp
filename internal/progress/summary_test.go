package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/humanize"
)

// The closing line reads as a sentence, agrees in number, and says what did
// not go to plan.
func TestSummaryReadsNaturally(t *testing.T) {
	r, _ := newTestReporter(t)
	var out bytes.Buffer
	r.Summary(&out, false, false)
	if s := out.String(); !strings.Contains(s, "Copied 3 files") || !strings.Contains(s, "256 KiB") {
		t.Errorf("summary = %q", s)
	}

	r, _ = newTestReporter(t)
	r.doneFiles.Store(1)
	r.skippedFiles.Store(2)
	r.failedFiles.Store(1)
	r.retries.Store(1)
	out.Reset()
	r.Summary(&out, true, false)
	s := out.String()
	for _, want := range []string{"Would copy 1 file ", "2 skipped", "1 failed", "1 transient error retried"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}
	if strings.Contains(s, "/s") {
		t.Errorf("a dry run reported a rate: %q", s)
	}

	// No display, no summary: cp is silent on success and scripts read stderr.
	out.Reset()
	New(Config{Mode: ModeNever}).Summary(&out, false, false)
	if out.Len() != 0 {
		t.Errorf("a disabled reporter wrote %q", out.String())
	}
}

func TestSummaryRetainsFinishedBarWithoutWaitingForRefresh(t *testing.T) {
	r, f := newTestReporter(t)
	r.interval = time.Hour
	r.plannedFiles.Store(1)
	r.plannedBytes.Store(100)
	r.doneFiles.Store(0)
	r.doneBytes.Store(0)
	r.Start()
	tk := r.Begin("last-file.txt", 100, DirLocal)
	tk.Set(42)
	r.paint.Lock()
	r.render()
	r.paint.Unlock()
	tk.Done(nil)
	_, at := written(t, f, 0)
	r.Summary(f, false, false)
	r.Stop() // deferred cleanup must not erase the settled frame
	r.Guard(func() { _, _ = f.WriteString("after summary\n") })
	got, _ := written(t, f, at)
	if !strings.Contains(got, "100%") || !strings.Contains(got, "Copied 1 file") || !strings.Contains(got, "██") {
		t.Fatalf("no completed frame: %q", got)
	}
	if strings.Contains(got, "last-file.txt") || strings.Contains(got, "ETA") || strings.Contains(got, "active") {
		t.Fatalf("final frame retained live state: %q", got)
	}
	if tail := got[strings.Index(got, "Copied 1 file"):]; strings.Contains(tail, eraseBelow) {
		t.Fatalf("completed frame was erased: %q", got)
	}
	if strings.Count(got, showCursor) != 1 || !strings.HasSuffix(got, "\nafter summary\n") {
		t.Fatalf("cursor was not restored on a fresh line: %q", got)
	}
}

func TestSummaryOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		planned     int64
		done        int64
		failed      int64
		skipped     int64
		interrupted bool
		dry         bool
		want        []string
	}{
		{name: "empty", want: []string{"Copied 0 files", "100%"}},
		{name: "skipped", skipped: 4, want: []string{"4 skipped", "100%"}},
		{name: "empty files", planned: 4, done: 4, want: []string{"Copied 4 files", "100%"}},
		{name: "failure", planned: 4, done: 3, failed: 1, want: []string{"✖", "1 failed", "75%"}},
		{name: "scan failure", failed: 1, want: []string{"✖", "1 failed", "  0%"}},
		{name: "interrupted", planned: 4, done: 1, interrupted: true, want: []string{"Interrupted", "25%"}},
		{name: "interrupted scan", interrupted: true, want: []string{"Interrupted", "  0%"}},
		{name: "dry run", planned: 4, done: 4, dry: true, want: []string{"Would copy 4 files", "100%"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newTestReporter(t)
			r.plannedFiles.Store(tt.planned)
			r.plannedBytes.Store(0)
			r.doneBytes.Store(0)
			r.doneFiles.Store(tt.done)
			r.Failed(tt.failed)
			r.Skipped(tt.skipped)
			var out bytes.Buffer
			r.Summary(&out, tt.dry, tt.interrupted)
			got := out.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("summary lacks %q: %s", want, got)
				}
			}
			if (tt.failed > 0 || tt.interrupted) && strings.Contains(got, "✔") {
				t.Errorf("unsuccessful copy displayed a success mark: %s", got)
			}
		})
	}
}

func TestSummaryFitsNarrowTerminals(t *testing.T) {
	r, _ := newTestReporter(t)
	r.Skipped(20000)
	r.Failed(1)
	r.retries.Store(1000)
	for _, level := range []colourLevel{levelNone, levelTrue} {
		r.pal = palette{level}
		for width := 1; width <= 120; width++ {
			r.width = width
			var out bytes.Buffer
			r.Summary(&out, false, true)
			for _, line := range strings.Split(out.String(), "\n") {
				if n := humanize.Width(stripANSI(line)); n >= width {
					t.Fatalf("summary is %d cells in a %d-cell terminal: %q", n, width, line)
				}
			}
		}
	}
}
