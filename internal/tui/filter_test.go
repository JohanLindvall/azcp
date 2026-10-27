// SPDX-License-Identifier: MIT

package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestNameFilterTerms(t *testing.T) {
	names := []string{"main.go", "Main_test.go", "README.md", "notes.txt", "-flag", "a.tar.gz"}
	for _, tc := range []struct{ filter, want string }{
		{"", "main.go Main_test.go README.md notes.txt -flag a.tar.gz"},
		{"MAIN", "main.go Main_test.go"},
		{"*.go", "main.go Main_test.go"},
		{"*.GO", ""},
		{"*.go -test", "main.go"},
		{"-*.go", "README.md notes.txt -flag a.tar.gz"},
		{"*.{md,txt}", "README.md notes.txt"},
		{"read note", "README.md notes.txt"},
		{"*.+(tar|gz)", "a.tar.gz"},
		{"[a-m]*", "main.go a.tar.gz"},
		{"-", "-flag"},
		{"*.go -*test*", "main.go"},
	} {
		f, err := parseFilter(tc.filter)
		if err != nil {
			t.Fatalf("%q: %v", tc.filter, err)
		}
		var got []string
		for _, n := range names {
			if f.match(n) {
				got = append(got, n)
			}
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%q shows %q, want %q", tc.filter, got, tc.want)
		}
	}
	for _, bad := range []string{"src/*.go", "**", "*/"} {
		if _, err := parseFilter(bad); err == nil {
			t.Errorf("%q was accepted as a name pattern", bad)
		}
	}
}

func TestPaneFilterIsPerPaneAndSurvivesNavigation(t *testing.T) {
	a, screen, left, right := testApp(t)
	write(t, filepath.Join(left, "keep.go"), "x")
	write(t, filepath.Join(left, "drop.txt"), "x")
	write(t, filepath.Join(left, "sub", "inner.go"), "x")
	write(t, filepath.Join(left, "sub", "inner.txt"), "x")
	write(t, filepath.Join(right, "other.txt"), "x")
	a.reload()
	finishJobs(t, a)
	p := &a.panes[0]
	press(a, tcell.KeyRune, ' ')
	press(a, tcell.KeyRune, '/')
	for _, r := range "*.go sub" {
		press(a, tcell.KeyRune, r)
	}
	press(a, tcell.KeyEnter, 0)
	if got := strings.Join(entryNames(p.entries), " "); got != ".. sub keep.go" || len(p.selected) != 0 || len(p.hidden) != 1 {
		t.Fatalf("filtered pane shows %q with marks %v", got, p.selected)
	}
	if got := strings.Join(entryNames(a.panes[1].entries), " "); got != ".. other.txt" {
		t.Fatalf("filter reached the other pane: %q", got)
	}
	a.draw()
	if !strings.Contains(screenText(screen), "2 of 3 shown · filter: *.go sub") {
		t.Fatal("footer does not show the filter or what it hides")
	}
	p.cursor = 1
	press(a, tcell.KeyEnter, 0)
	finishJobs(t, a)
	if got := strings.Join(entryNames(p.entries), " "); got != ".. inner.go" || p.cursor != 1 {
		t.Fatalf("filter lost on entering a directory: %q, cursor %d", got, p.cursor)
	}
	press(a, tcell.KeyRune, '*')
	if got := p.selection(); len(got) != 1 || got[0].name != "inner.go" {
		t.Fatalf("selection reached hidden entries: %v", entryNames(got))
	}
	press(a, tcell.KeyRune, '/')
	press(a, tcell.KeyCtrlU, 0)
	press(a, tcell.KeyEnter, 0)
	if got := strings.Join(entryNames(p.entries), " "); got != ".. inner.go inner.txt" || len(p.selected) != 0 || a.modal != nil {
		t.Fatalf("cleared filter shows %q", got)
	}
}

func TestPaneFilterRejectsAPathAndKeepsThePrompt(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "file"), "x")
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, '/')
	for _, r := range "a/*" {
		press(a, tcell.KeyRune, r)
	}
	press(a, tcell.KeyEnter, 0)
	if a.modal == nil || !strings.HasPrefix(a.status, "Error:") || len(a.panes[0].entries) != 2 {
		t.Fatalf("path pattern accepted: status %q", a.status)
	}
}
