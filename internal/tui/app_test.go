// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func testApp(t *testing.T) (*app, tcell.SimulationScreen, string, string) {
	t.Helper()
	b := testBackend(t)
	left, right := t.TempDir(), t.TempDir()
	b.opt.TUIPaths = [2]string{left, right}
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	s.SetSize(100, 30)
	t.Cleanup(s.Fini)
	a, err := newApp(t.Context(), s, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if a.busy != nil {
			a.busy.cancel()
		}
		a.workers.Wait()
	})
	return a, s, left, right
}

func press(a *app, key tcell.Key, r rune) {
	a.key(tcell.NewEventKey(key, r, tcell.ModNone))
}

func finishJobs(t *testing.T, a *app) {
	t.Helper()
	for a.busy != nil {
		j := a.busy
		select {
		case r := <-j.done:
			j.cancel()
			a.busy = nil
			if r.err != nil {
				a.report(r.err)
			}
			j.finish(r)
		case <-time.After(5 * time.Second):
			t.Fatal("TUI job did not finish")
		}
	}
}

func screenText(s tcell.SimulationScreen) string {
	cells, width, _ := s.GetContents()
	var out strings.Builder
	for i, cell := range cells {
		for _, r := range cell.Runes {
			out.WriteRune(r)
		}
		if (i+1)%width == 0 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func TestKeyboardCopyMoveAndDeleteRequireConfirmation(t *testing.T) {
	a, _, left, right := testApp(t)
	write(t, filepath.Join(left, "a"), "a contents")
	write(t, filepath.Join(left, "b"), "b contents")
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, ' ')
	press(a, tcell.KeyRune, ' ')
	if got := len(a.panes[0].selection()); got != 2 {
		t.Fatalf("marked %d entries", got)
	}
	press(a, tcell.KeyF5, 0)
	press(a, tcell.KeyEnter, 0)
	if a.modal == nil || a.modal.kind != "confirm" || a.busy != nil {
		t.Fatal("copy started before confirmation")
	}
	press(a, tcell.KeyEnter, 0) // default is cancel
	if a.modal != nil || a.busy != nil {
		t.Fatal("Enter did not cancel confirmation")
	}
	if _, err := os.Stat(filepath.Join(right, "a")); !os.IsNotExist(err) {
		t.Fatal("cancelled copy wrote data")
	}
	press(a, tcell.KeyF5, 0)
	press(a, tcell.KeyEnter, 0)
	press(a, tcell.KeyRune, 'y')
	finishJobs(t, a)
	read(t, filepath.Join(right, "a"), "a contents")
	read(t, filepath.Join(right, "b"), "b contents")
	// Move replaces the existing copy only after another explicit confirmation.
	press(a, tcell.KeyF6, 0)
	press(a, tcell.KeyEnter, 0)
	press(a, tcell.KeyRune, 'y')
	finishJobs(t, a)
	if _, err := os.Stat(filepath.Join(left, "a")); !os.IsNotExist(err) {
		t.Fatalf("move left source: %v", err)
	}
	press(a, tcell.KeyTab, 0)
	press(a, tcell.KeyF8, 0)
	press(a, tcell.KeyEscape, 0)
	read(t, filepath.Join(right, "a"), "a contents")
	press(a, tcell.KeyF8, 0)
	press(a, tcell.KeyRune, 'y')
	finishJobs(t, a)
	if _, err := os.Stat(filepath.Join(right, "a")); !os.IsNotExist(err) {
		t.Fatalf("delete left target: %v", err)
	}
	read(t, filepath.Join(right, "b"), "b contents")
}

func TestParentCannotBeSelectedForDeletion(t *testing.T) {
	a, _, _, _ := testApp(t)
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyHome, 0)
	press(a, tcell.KeyRune, ' ')
	press(a, tcell.KeyF8, 0)
	if a.modal != nil || a.busy != nil {
		t.Fatal("parent was actionable")
	}
}

func TestPastedKeysCannotConfirmOrStartOperations(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "file"), "keep")
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyF8, 0)
	a.pasting = true
	press(a, tcell.KeyRune, 'y')
	press(a, tcell.KeyEnter, 0)
	if a.busy != nil || a.modal == nil {
		t.Fatal("paste confirmed deletion")
	}
	a.pasting = false
	press(a, tcell.KeyEscape, 0)
	press(a, tcell.KeyF4, 0)
	a.pasting = true
	for _, r := range "path with spaces" {
		press(a, tcell.KeyRune, r)
	}
	press(a, tcell.KeyEnter, 0)
	if a.modal == nil || a.modal.text != "path with spaces" || a.busy != nil {
		t.Fatal("paste submitted the path prompt or lost text")
	}
	read(t, filepath.Join(left, "file"), "keep")
}

func TestRenderingResizesAndRedactsUntrustedText(t *testing.T) {
	a, s, left, _ := testApp(t)
	u, err := uri.Parse("azure://account/c?sig=SECRET", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.setEntries(0, loc(t, left), []entry{{name: "wide 界 é 👩‍💻\x1b[2J\u202e", loc: location{url: u}}})
	a.logs.add("Sign in at https://example.test/?sig=OTHERSECRET")
	for _, size := range [][2]int{{100, 30}, {50, 14}, {20, 5}, {120, 40}} {
		s.SetSize(size[0], size[1])
		a.draw()
		text := screenText(s)
		if strings.Contains(text, "SECRET") || strings.ContainsAny(text, "\x1b\u202e") {
			t.Fatalf("unsafe text reached screen: %q", text)
		}
		if !strings.Contains(text, "azcp") {
			t.Fatalf("screen missing title at %v", size)
		}
	}
}

type blockingList struct {
	store.Store
	started chan struct{}
}

func (b blockingList) ReadDir(ctx context.Context, _ *uri.URL) ([]*store.Node, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestSlowListingRemainsCancellable(t *testing.T) {
	a, _, _, _ := testApp(t)
	started := make(chan struct{})
	a.backend.local = blockingList{Store: a.backend.local, started: started}
	a.navigate(a.panes[0].loc)
	<-started
	press(a, tcell.KeyF8, 0)
	if a.modal != nil {
		t.Fatal("destructive action accepted during listing")
	}
	press(a, tcell.KeyCtrlC, 0)
	finishJobs(t, a)
	if !strings.Contains(a.status, "Cancelled") || a.quit {
		t.Fatalf("cancel result: %s; quit=%v", a.status, a.quit)
	}
}

func TestRunQuitsAndJoinsWorkers(t *testing.T) {
	a, s, _, _ := testApp(t)
	done := make(chan error, 1)
	go func() { done <- a.run() }()
	s.InjectKey(tcell.KeyF10, 0, tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not release workers or input reader")
	}
}
