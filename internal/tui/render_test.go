// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
)

func TestProgressDoesNotClaimCompletionDuringScanning(t *testing.T) {
	a, screen, _, _ := testApp(t)
	p := progress.New(progress.Config{Mode: progress.ModeNever})
	t.Cleanup(p.Stop)
	p.Plan(1, 1000)
	task := p.Begin("large-file", 1000, progress.DirUpload)
	task.Set(999)
	p.SetScanning(true)
	a.busy = &job{label: "Copy", progress: p, cancel: func() {}}
	a.draw()
	if text := screenText(screen); !strings.Contains(text, "scanning") || strings.Contains(text, "%") {
		t.Fatalf("provisional total became a percentage:\n%s", text)
	}
	p.SetScanning(false)
	a.draw()
	if text := screenText(screen); !strings.Contains(text, "99%") || strings.Contains(text, "100%") {
		t.Fatalf("unfinished copy claimed completion:\n%s", text)
	}
	task.Done(nil)
	a.draw()
	if !strings.Contains(screenText(screen), "100%") {
		t.Fatal("completed copy did not fill the meter")
	}
}

func TestNarrowProgressKeepsFailuresAndRetriesVisible(t *testing.T) {
	a, screen, _, _ := testApp(t)
	screen.SetSize(50, 14)
	p := progress.New(progress.Config{Mode: progress.ModeNever})
	t.Cleanup(p.Stop)
	p.Failed(2)
	p.Skipped(3)
	task := p.Begin("file", 100, progress.DirUpload)
	task.Retrying(2, 3, time.Second)
	a.busy = &job{label: "Copy", progress: p, cancel: func() {}}
	a.draw()
	for _, want := range []string{"2 failed", "1 retried", "3 skipped"} {
		if !strings.Contains(screenText(screen), want) {
			t.Fatalf("narrow status hid %q:\n%s", want, screenText(screen))
		}
	}
}

func TestMinimumSizeKeepsDestinationAndInputVisible(t *testing.T) {
	a, screen, left, right := testApp(t)
	write(t, filepath.Join(left, "file"), "payload")
	a.reload()
	finishJobs(t, a)
	screen.SetSize(50, 14)
	press(a, tcell.KeyF5, 0)
	a.modal.target = loc(t, filepath.Join(right, "target-folder"))
	a.draw()
	text := screenText(screen)
	for _, want := range []string{"To:", "target-folder", "Existing files", "e: edit", "cancel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("confirmation hid %q:\n%s", want, text)
		}
	}
	press(a, tcell.KeyRune, 'e')
	a.draw()
	x, y, visible := screen.GetCursor()
	if !visible || x < 0 || x >= 50 || y < 0 || y >= 14 {
		t.Fatalf("destination editor lost its cursor: %d,%d visible=%v", x, y, visible)
	}
	press(a, tcell.KeyEscape, 0)
	press(a, tcell.KeyF8, 0)
	a.draw()
	if !strings.Contains(screenText(screen), "Deletion is permanent") {
		t.Fatal("small deletion dialog hid the permanent-delete warning")
	}
}

func TestMonochromeKeepsFocusAndSelectionWithoutColors(t *testing.T) {
	t.Setenv("NO_COLOR", "") // Even an empty value opts out, as in cp mode.
	a, screen, left, _ := testApp(t)
	write(t, filepath.Join(left, "file"), "payload")
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, ' ')
	a.draw()
	text := screenText(screen)
	if !strings.Contains(text, "›●") || !strings.Contains(text, "1 marked") {
		t.Fatalf("monochrome lost its selection cues:\n%s", text)
	}
	press(a, tcell.KeyF5, 0)
	a.draw()
	cells, _, _ := screen.GetContents()
	for _, cell := range cells {
		fg, bg, _ := cell.Style.Decompose()
		if fg != tcell.ColorDefault || bg != tcell.ColorDefault {
			t.Fatalf("NO_COLOR emitted explicit colors: fg=%v bg=%v", fg, bg)
		}
	}
}

func TestPageNavigationKeepsCursorInResizedPane(t *testing.T) {
	a, screen, left, _ := testApp(t)
	var entries []entry
	for i := range 40 {
		name := fmt.Sprintf("file-%02d", i)
		entries = append(entries, entry{name: name, loc: loc(t, filepath.Join(left, name)), node: &store.Node{Kind: store.KindFile}})
	}
	a.setEntries(0, loc(t, left), entries)
	for _, size := range [][2]int{{132, 30}, {50, 14}, {80, 24}} {
		screen.SetSize(size[0], size[1])
		press(a, tcell.KeyPgDn, 0)
		a.draw()
		p := a.panes[0]
		if p.cursor < p.offset || p.cursor >= p.offset+paneRows(size[1]) || !strings.Contains(screenText(screen), p.entries[p.cursor].name) {
			t.Fatalf("cursor left the viewport after resizing to %v", size)
		}
	}
}

func TestMeterHandlesUnknownAndEmptyFileTotals(t *testing.T) {
	a, screen, _, _ := testApp(t)
	for _, test := range []struct {
		snapshot progress.Snapshot
		want     string
	}{
		{progress.Snapshot{Done: 1, PlannedFiles: 4}, "25%"},
		{progress.Snapshot{Bytes: 100, Elapsed: time.Second}, "100 B/s"},
	} {
		screen.Clear()
		a.drawMeter(0, 0, 80, test.snapshot)
		screen.Show()
		if !strings.Contains(screenText(screen), test.want) {
			t.Fatalf("meter missing %q: %s", test.want, screenText(screen))
		}
	}
	// A cancellation message still outranks a previous successful status.
	a.report(context.Canceled)
	a.draw()
	if !strings.Contains(screenText(screen), "Cancelled") {
		t.Fatal("cancellation status disappeared")
	}
}
