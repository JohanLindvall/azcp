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

func TestNameFilterTerms(t *testing.T) {
	names := []string{"main.go", "Main_test.go", "README.md", "notes.txt", "-flag", "a.tar.gz", "old-notes.txt", "[draft].md"}
	for _, tc := range []struct{ filter, want string }{
		{"", "main.go Main_test.go README.md notes.txt -flag a.tar.gz old-notes.txt [draft].md"},
		{"MAIN", "main.go Main_test.go"},
		{"*.go", "main.go Main_test.go"},
		{"*.GO", ""},
		{"*.go -test", "main.go"},
		{"-*.go", "README.md notes.txt -flag a.tar.gz old-notes.txt [draft].md"},
		{"*.{md,txt}", "README.md notes.txt old-notes.txt [draft].md"},
		{"read note", "README.md notes.txt old-notes.txt"},
		{"*.+(tar|gz)", "a.tar.gz"},
		{"[a-m]*", "main.go a.tar.gz"},
		{"-", "-flag old-notes.txt"},
		{"*.go -*test*", "main.go"},
		{"name:-flag", "-flag"},
		{"notes.txt", "notes.txt old-notes.txt"},
		{"name=notes.txt", "notes.txt"},
		{"NAME=notes.txt", "notes.txt"},
		{"name=NOTES.TXT", ""},
		{"name=[draft].md", "[draft].md"},
		{`\[draft\].md`, "[draft].md"},
		{"name=*.go", ""},
		{"name=main.go name=README.md", "main.go README.md"},
		{"*.txt -name=notes.txt", "old-notes.txt"},
		{"has=notes.txt", "notes.txt"},
		{`"notes.txt" main.go`, "main.go notes.txt old-notes.txt"},
		{"has:.go", "main.go Main_test.go"},
		{"type:file *.md", "README.md [draft].md"},
	} {
		f, err := parseFilter(tc.filter)
		if err != nil {
			t.Fatalf("%q: %v", tc.filter, err)
		}
		var got []string
		for _, n := range names {
			if f.match(entry{name: n, node: &store.Node{Kind: store.KindFile}}, false) {
				got = append(got, n)
			}
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%q shows %q, want %q", tc.filter, got, tc.want)
		}
	}
	for _, bad := range []string{"src/*.go", "a/b", "**", "*/", ">10Q", "**/*.mp4", "sise>5M", "foo:bar",
		`"open`, "type:x", "files>x", "older:soon", "has:", "has>1", "name=", "has="} {
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
	if got := strings.Join(entryNames(p.entries), " "); got != ".. sub keep.go" || !p.selected[1] || len(p.selected) != 1 || len(p.hidden) != 1 {
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
	if got := strings.Join(entryNames(p.entries), " "); got != ".. inner.go" || p.cursor != 1 || len(p.selected) != 0 {
		t.Fatalf("filter lost on entering a directory: %q, cursor %d", got, p.cursor)
	}
	press(a, tcell.KeyRune, '*')
	if got := p.selection(); len(got) != 1 || got[0].name != "inner.go" {
		t.Fatalf("selection reached hidden entries: %v", entryNames(got))
	}
	press(a, tcell.KeyRune, '/')
	press(a, tcell.KeyBackspace2, 0)
	if a.modal.text != "*.go su" {
		t.Fatalf("Backspace did not edit the offered filter: %q", a.modal.text)
	}
	press(a, tcell.KeyCtrlU, 0)
	press(a, tcell.KeyEnter, 0)
	if got := strings.Join(entryNames(p.entries), " "); got != ".. inner.go inner.txt" || !p.selected[1] || p.selected[2] || a.modal != nil {
		t.Fatalf("cleared filter shows %q, marks %v, modal %v", got, p.selected, a.modal)
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

func TestFilterBySizeAndContentsMeasuresWhatItNeeds(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "big", "deep", "a.mp4"), strings.Repeat("x", 2048))
	write(t, filepath.Join(left, "small", "b.mp4"), "x")
	write(t, filepath.Join(left, "other", "c.txt"), strings.Repeat("x", 4096))
	write(t, filepath.Join(left, "clip.mp4"), strings.Repeat("x", 3000))
	write(t, filepath.Join(left, "tiny.mp4"), "x")
	old, recent := time.Unix(1_600_000_000, 0), time.Unix(1_700_000_000, 0)
	for name, when := range map[string]time.Time{"big/deep/a.mp4": old, "other/c.txt": recent, "clip.mp4": old} {
		if err := os.Chtimes(filepath.Join(left, name), when, when); err != nil {
			t.Fatal(err)
		}
	}
	a.reload()
	finishJobs(t, a)
	p := &a.panes[0]
	a.applyFilter(">1k has:*.mp4")
	if a.busy == nil {
		t.Fatal("a size and contents filter did not measure the directories it needs")
	}
	finishJobs(t, a)
	if got := strings.Join(entryNames(p.entries), " "); got != ".. big clip.mp4" {
		t.Fatalf(">1k has:*.mp4 shows %q", got)
	}
	a.applyFilter("-has:*.mp4")
	if a.busy != nil {
		t.Fatal("measured again what the last walk already answered")
	}
	if got := strings.Join(entryNames(p.entries), " "); got != ".. other" {
		t.Fatalf("-has:*.mp4 shows %q", got)
	}
	for filter, want := range map[string]string{
		"older:2022-01-01":                     ".. big clip.mp4",
		"type:dir files>=1 newer:2022-06-01":   ".. other small",
		"type:dir -older:30d":                  ".. small",
		`newer:"2019-01-01 00:00:00" type:dir`: ".. big other small",
	} {
		a.applyFilter(filter)
		if got := strings.Join(entryNames(p.entries), " "); got != want || a.busy != nil {
			t.Errorf("%s shows %q, want %q", filter, got, want)
		}
	}
	a.applyFilter("")
	p.sort = paneSort{field: sortNewest, reverse: true, mixDirectories: true}
	p.sortEntries()
	// small and tiny.mp4 were both written just now, in no particular order.
	if got := strings.Join(entryNames(p.entries[3:]), " "); got != "other clip.mp4 big" {
		t.Fatalf("newest first gives %q", got)
	}
	if !p.entries[3].newest().Equal(recent) {
		t.Fatalf("other's newest file is %v, want %v", p.entries[3].newest(), recent)
	}
}

func TestPaneMeasurementDoesNotWalkHiddenDirectories(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "keep", "file"), "data")
	write(t, filepath.Join(left, "excluded", "file"), "unrelated")
	a.reload()
	finishJobs(t, a)
	local := a.backend.local
	var walked []string
	a.backend.local = &sizeWalkStore{Store: local, scan: func(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
		walked = append(walked, u.Path)
		return local.WalkAll(ctx, u, onError, fn)
	}}
	a.applyFilter("name=keep size>0")
	finishJobs(t, a)
	if len(walked) != 1 || walked[0] != filepath.ToSlash(filepath.Join(left, "keep")) {
		t.Fatalf("walked %v, want only the visible directory", walked)
	}
	p := &a.panes[0]
	if len(p.entries) != 2 || p.entries[1].name != "keep" || p.entries[1].dirSize.bytes != 4 {
		t.Fatalf("filtered entries: %v", entryNames(p.entries))
	}
}

func TestUsageFilterWeighsTheCachedTree(t *testing.T) {
	a, _, left, _ := testApp(t)
	// Logical totals here include directories' own lengths, a few KiB each.
	write(t, filepath.Join(left, "videos", "x", "a.MP4"), strings.Repeat("x", 1<<20))
	write(t, filepath.Join(left, "docs", "a.txt"), strings.Repeat("x", 4096))
	write(t, filepath.Join(left, "mixed", "a.mp4"), "x")
	press(a, tcell.KeyRune, 'u')
	finishJobs(t, a)
	p := &a.panes[0]
	a.applyFilter("has:*.{mp4,MP4} size:>=512K")
	if a.busy != nil {
		t.Fatal("the usage view walked again instead of reading its cache")
	}
	if got := strings.Join(entryNames(p.entries), " "); got != ".. videos" {
		t.Fatalf("usage filter shows %q", got)
	}
}

func TestSinglePaneBrowsesAndDeletesButNeverCopies(t *testing.T) {
	a, screen, left, _ := testApp(t)
	a.single = true
	write(t, filepath.Join(left, "file"), "x")
	a.reload()
	finishJobs(t, a)
	if a.panes[1].entries != nil {
		t.Fatal("single-pane mode listed a second pane")
	}
	for _, key := range []rune{'c', 'm'} {
		press(a, tcell.KeyRune, key)
		if a.modal != nil || !strings.Contains(a.status, "single-pane") {
			t.Fatalf("%c offered a copy: %q", key, a.status)
		}
	}
	press(a, tcell.KeyTab, 0)
	if a.active != 0 {
		t.Fatal("Tab reached a pane that is not shown")
	}
	a.draw()
	if text := screenText(screen); strings.Count(text, "LOCAL") != 1 || !strings.Contains(text, "Modified") {
		t.Fatalf("single pane is not one full-width pane with dates:\n%s", text)
	}
	press(a, tcell.KeyRune, 'd')
	if a.modal == nil || a.modal.action != "Delete" {
		t.Fatal("single-pane mode cannot delete")
	}
	press(a, tcell.KeyRune, 'y')
	finishJobs(t, a)
	if _, err := os.Stat(filepath.Join(left, "file")); !os.IsNotExist(err) {
		t.Fatalf("delete left the file: %v", err)
	}
}

// The fewest keys from an account root to every container holding an .mp4
// whose newest file is over 60 days old, all marked: / filter Enter *.
func TestFilterThenMarkAllTakesOneKeyAfterTheFilter(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "old", "v", "a.mp4"), "x")
	write(t, filepath.Join(left, "fresh", "a.mp4"), "x")
	write(t, filepath.Join(left, "stale", "a.txt"), "x")
	long := time.Now().AddDate(0, 0, -90)
	for _, name := range []string{"old/v/a.mp4", "stale/a.txt"} {
		if err := os.Chtimes(filepath.Join(left, name), long, long); err != nil {
			t.Fatal(err)
		}
	}
	a.reload()
	finishJobs(t, a)
	p := &a.panes[0]
	typeText := func(s string) {
		for _, r := range s {
			press(a, tcell.KeyRune, r)
		}
	}
	press(a, tcell.KeyRune, '/')
	typeText("something else")
	press(a, tcell.KeyEnter, 0)
	press(a, tcell.KeyRune, '/')
	typeText("has:.mp4 older:60d") // replaces the offered filter
	press(a, tcell.KeyEnter, 0)
	finishJobs(t, a)
	press(a, tcell.KeyRune, '*')
	if got := entryNames(p.selection()); strings.Join(got, " ") != "old" {
		t.Fatalf("marked %v", got)
	}
}
