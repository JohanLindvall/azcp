// SPDX-License-Identifier: MIT

package tui

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/store"
)

func entryNames(entries []entry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names
}

func TestPaneSortFieldsAndDirections(t *testing.T) {
	file := func(name string, size, date int64) entry {
		return entry{name: name, node: &store.Node{Kind: store.KindFile, Size: size, ModTime: time.Unix(date, 0)}}
	}
	entries := []entry{
		{name: ".."},
		{name: "z-dir", node: &store.Node{Kind: store.KindDir, Size: 4096}},
		file("z.txt", 2, 1), file("a.go", 4, 3), file("b.txt", 2, 3),
		{name: "a-dir", node: &store.Node{Kind: store.KindDir, ModTime: time.Unix(2, 0)}, dirSize: &directorySize{bytes: 5}},
	}
	for _, tc := range []struct {
		name string
		sort paneSort
		want string
	}{
		{"name", paneSort{}, ".. a-dir z-dir a.go b.txt z.txt"},
		{"name reverse", paneSort{field: sortName, reverse: true}, ".. z-dir a-dir z.txt b.txt a.go"},
		{"modified", paneSort{field: sortModified}, ".. a-dir z-dir z.txt a.go b.txt"},
		{"modified reverse", paneSort{field: sortModified, reverse: true}, ".. a-dir z-dir b.txt a.go z.txt"},
		{"size", paneSort{field: sortSize}, ".. a-dir z-dir b.txt z.txt a.go"},
		{"size reverse", paneSort{field: sortSize, reverse: true}, ".. a-dir z-dir a.go z.txt b.txt"},
		{"extension", paneSort{field: sortExtension}, ".. a-dir z-dir a.go b.txt z.txt"},
		{"extension reverse", paneSort{field: sortExtension, reverse: true}, ".. z-dir a-dir z.txt b.txt a.go"},
		{"mixed size", paneSort{field: sortSize, mixDirectories: true}, ".. b.txt z.txt a.go a-dir z-dir"},
		{"mixed size reverse", paneSort{field: sortSize, mixDirectories: true, reverse: true}, ".. a-dir a.go z.txt b.txt z-dir"},
		{"mixed modified reverse", paneSort{field: sortModified, mixDirectories: true, reverse: true}, ".. b.txt a.go a-dir z.txt z-dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pane{entries: slices.Clone(entries), sort: tc.sort}
			p.sortEntries()
			if got := strings.Join(entryNames(p.entries), " "); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if p.cursor != 0 {
				t.Fatal("sorting moved focus off the parent")
			}
		})
	}
}

func TestSortExtensionsAndTypes(t *testing.T) {
	for _, tc := range []struct {
		name, want string
	}{
		{"README", ""}, {".env", ""}, {".env.local", "local"},
		{"a.TXT", "txt"}, {"archive.tar.gz", "gz"}, {"trailing.", ""},
	} {
		if got := extension(entry{name: tc.name, node: &store.Node{Kind: store.KindFile}}); got != tc.want {
			t.Errorf("extension of %q = %q, want %q", tc.name, got, tc.want)
		}
	}
	p := pane{sort: paneSort{field: sortType, mixDirectories: true}, entries: []entry{
		{name: ".."},
		{name: "a-link", node: &store.Node{Kind: store.KindSymlink}},
		{name: "b-device", node: &store.Node{Kind: store.KindOther}},
		{name: "z-file", node: &store.Node{Kind: store.KindFile}},
		{name: "c-dir", node: &store.Node{Kind: store.KindDir}},
	}}
	p.sortEntries()
	if got := strings.Join(entryNames(p.entries), " "); got != ".. c-dir z-file b-device a-link" {
		t.Fatalf("sort by type: %s", got)
	}
	p.sort.reverse = true
	p.sortEntries()
	if got := strings.Join(entryNames(p.entries), " "); got != ".. a-link b-device z-file c-dir" {
		t.Fatalf("sort by type reversed: %s", got)
	}
}

func TestSortByRecursiveFileCount(t *testing.T) {
	entries := []entry{
		{name: ".."},
		{name: "large", node: &store.Node{Kind: store.KindDir}, dirSize: &directorySize{bytes: 100, files: 1}},
		{name: "many", node: &store.Node{Kind: store.KindDir}, dirSize: &directorySize{bytes: 2, files: 2}},
		{name: "empty", node: &store.Node{Kind: store.KindDir}, dirSize: &directorySize{}},
		{name: "unknown", node: &store.Node{Kind: store.KindDir, Size: 4096}},
		{name: "file", node: &store.Node{Kind: store.KindFile, Size: 999}},
	}
	for _, tc := range []struct {
		reverse, mix bool
		want         string
	}{
		{false, false, ".. empty large many unknown file"},
		{true, false, ".. many large empty unknown file"},
		{false, true, ".. empty file large many unknown"},
		{true, true, ".. many large file empty unknown"},
	} {
		p := pane{entries: slices.Clone(entries), sort: paneSort{field: sortFiles, reverse: tc.reverse, mixDirectories: tc.mix}}
		p.sortEntries()
		if got := strings.Join(entryNames(p.entries), " "); got != tc.want {
			t.Errorf("reverse=%v, mix=%v: %q; want %q", tc.reverse, tc.mix, got, tc.want)
		}
	}
	for _, kind := range []store.Kind{store.KindSymlink, store.KindOther} {
		if n, known := (entry{node: &store.Node{Kind: kind}}).fileCount(); !known || n != 0 {
			t.Fatalf("%s counted as a regular file: %d, %v", kind, n, known)
		}
	}
}

func TestSortingPreservesFocusAndMarksForSameNameBlobAndPrefix(t *testing.T) {
	dir, blob := &store.Node{Kind: store.KindDir}, &store.Node{Kind: store.KindFile}
	p := pane{cursor: 1, selected: map[int]bool{2: true}, sort: paneSort{field: sortType, reverse: true, mixDirectories: true},
		entries: []entry{{name: ".."}, {name: "same", node: dir}, {name: "same", node: blob}}}
	p.sortEntries()
	if p.cursor != 2 || p.entries[p.cursor].node != dir || !p.selected[1] || p.selected[2] || p.selection()[0].node != blob {
		t.Fatal("sorting changed the focused or marked object")
	}
	p.sort = paneSort{}
	p.sortEntries()
	if p.cursor != 1 || !p.selected[2] || p.selected[1] || p.selection()[0].node != blob {
		t.Fatal("sorting back changed the focused or marked object")
	}
}

func TestSortMenuIsPerPaneAndSurvivesNavigation(t *testing.T) {
	a, screen, left, _ := testApp(t)
	write(t, filepath.Join(left, "dir", "file"), "payload")
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, 's')
	for _, size := range [][2]int{{100, 30}, {50, 14}} {
		screen.SetSize(size[0], size[1])
		a.draw()
		for _, text := range []string{"1 Name", "2 Modified", "3 Size", "4 Extension", "5 Type", "6 Files", "r Reverse", "d Dirs first", "f Column", "Enter/Esc: close"} {
			if !strings.Contains(screenText(screen), text) {
				t.Fatalf("sort menu at %v hides %q:\n%s", size, text, screenText(screen))
			}
		}
	}
	press(a, tcell.KeyRune, '3')
	press(a, tcell.KeyRune, 'r')
	press(a, tcell.KeyRune, 'd')
	want := paneSort{field: sortSize, reverse: true, mixDirectories: true}
	if a.panes[0].sort != want || a.panes[1].sort != (paneSort{}) || a.busy != nil {
		t.Fatal("sort menu changed the other pane or started filesystem work")
	}
	press(a, tcell.KeyEscape, 0)
	press(a, tcell.KeyEnter, 0)
	finishJobs(t, a)
	wantPath := loc(t, filepath.Join(left, "dir")).url.Path
	if a.panes[0].loc.url.Path != wantPath {
		t.Fatalf("navigation reached %q, want %q", a.panes[0].loc.url.Path, wantPath)
	}
	if a.panes[0].sort != want {
		t.Fatal("navigation lost sort settings")
	}
	press(a, tcell.KeyCtrlR, 0)
	finishJobs(t, a)
	if a.panes[0].sort != want || a.panes[1].sort != (paneSort{}) {
		t.Fatal("refresh lost per-pane sort settings")
	}
}
