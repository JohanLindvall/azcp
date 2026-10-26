// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type sizeWalkStore struct {
	store.Store
	walks int
	scan  func(context.Context, *uri.URL, func(*uri.URL, error) error, func(*store.Node) error) error
}

func (s *sizeWalkStore) WalkAll(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
	s.walks++
	if s.scan != nil {
		return s.scan(ctx, u, onError, fn)
	}
	return s.Store.WalkAll(ctx, u, onError, fn)
}

func TestLocalDirectorySizesAreRecursiveLogicalLengths(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "tree", "nested", "file"), "hello")
	write(t, filepath.Join(root, "tree", "sparse"), "")
	if err := os.Truncate(filepath.Join(root, "tree", "sparse"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "direct-file"), "not in a subdirectory")
	entries := []entry{selected(t, b, filepath.Join(root, "tree")), selected(t, b, filepath.Join(root, "empty"))}
	want := []directorySize{{bytes: (1 << 20) + 5, files: 2}, {}}
	for _, all := range []bool{false, true} {
		walk := &sizeWalkStore{Store: b.local}
		b.local = walk
		p := reporter(t)
		sizes, err := b.directorySizes(t.Context(), loc(t, root), entries, all, p)
		if err != nil || !slices.Equal(sizes, want) {
			t.Fatalf("all=%v: sizes=%v, err=%v; want %v", all, sizes, err, want)
		}
		walks := 2
		if all {
			walks = 1
		}
		if walk.walks != walks || p.Snapshot().PlannedBytes != want[0].bytes || p.Snapshot().PlannedFiles != 2 {
			t.Fatalf("all=%v: walks=%d, progress=%+v", all, walk.walks, p.Snapshot())
		}
		b.local = walk.Store
	}
}

func TestDirectorySizesDoNotFollowSymlinksAndCountHardLinkNames(t *testing.T) {
	b := testBackend(t)
	root, outside := t.TempDir(), t.TempDir()
	tree := filepath.Join(root, "tree")
	write(t, filepath.Join(tree, "file"), "hello")
	write(t, filepath.Join(outside, "file"), "outside")
	if err := os.Link(filepath.Join(tree, "file"), filepath.Join(tree, "hardlink")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(tree, filepath.Join(tree, "loop")); err != nil {
		t.Fatal(err)
	}
	entries := []entry{selected(t, b, tree)}
	for _, all := range []bool{false, true} {
		sizes, err := b.directorySizes(t.Context(), loc(t, root), entries, all, reporter(t))
		if err != nil || !slices.Equal(sizes, []directorySize{{bytes: 10, files: 2}}) {
			t.Fatalf("all=%v: sizes=%v, err=%v", all, sizes, err)
		}
	}
	// A directory that became a symlink since the pane loaded must not be
	// passed to WalkAll, whose root lookup otherwise follows links.
	if err := os.Rename(tree, filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tree); err != nil {
		t.Fatal(err)
	}
	sizes, err := b.directorySizes(t.Context(), loc(t, root), entries, false, reporter(t))
	if err == nil || sizes != nil {
		t.Fatalf("followed a replaced root: %v, %v", sizes, err)
	}
}

type sizeBlobs struct {
	blobStore
	nodes []*store.Node
	walks []*uri.URL
}

func (s *sizeBlobs) WalkAll(ctx context.Context, u *uri.URL, _ func(*uri.URL, error) error, fn func(*store.Node) error) error {
	s.walks = append(s.walks, u)
	for _, n := range s.nodes {
		if u.Container != "" && n.URL.Container != u.Container || u.Key != "" && !strings.HasPrefix(n.URL.Key, u.Key+"/") {
			continue
		}
		if err := fn(n); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func TestAzureDirectorySizesUseFlatListingsAndExactPrefixes(t *testing.T) {
	b := testBackend(t)
	base := loc(t, "azure://account/c/prefix?sig=SECRET")
	remote := &sizeBlobs{}
	b.remote = remote // Unimplemented operations panic: no stats or file reads.
	add := func(key string, kind store.Kind, size int64) entry {
		u := base.url.WithPathPart("c/" + key)
		n := &store.Node{URL: u, Kind: kind, Size: size}
		remote.nodes = append(remote.nodes, n)
		return entry{name: key, loc: location{url: u}, node: n}
	}
	var entries []entry
	for _, key := range []string{"prefix/a", "prefix/", "prefix/.", "prefix/..", "prefix/empty"} {
		entries = append(entries, add(key, store.KindDir, 0))
	}
	add("prefix/a", store.KindFile, 999) // Independent blob with the same name.
	add("prefix/a/file", store.KindFile, 7)
	add("prefix/a/deep/file", store.KindFile, 11)
	add("prefix//file", store.KindFile, 13)
	add("prefix/./file", store.KindFile, 17)
	add("prefix/../file", store.KindFile, 19)
	add("prefix/a/link", store.KindSymlink, 99)
	add("prefix/a/device", store.KindOther, 99)
	add("prefix/direct-file", store.KindFile, 99)
	add("prefix-sibling/file", store.KindFile, 99)
	want := []directorySize{{18, 2}, {13, 1}, {17, 1}, {19, 1}, {}}
	for _, all := range []bool{false, true} {
		remote.walks = nil
		sizes, err := b.directorySizes(t.Context(), base, entries, all, reporter(t))
		if err != nil || !slices.Equal(sizes, want) {
			t.Fatalf("all=%v: sizes=%v, err=%v; want %v", all, sizes, err, want)
		}
		walks := len(entries)
		if all {
			walks = 1
		}
		if len(remote.walks) != walks {
			t.Fatalf("all=%v: got %d walks, want %d", all, len(remote.walks), walks)
		}
		for i, u := range remote.walks {
			key := base.url.Key
			if !all {
				key = entries[i].loc.url.Key
			}
			if u.Key != key || u.SAS != base.url.SAS {
				t.Fatal("walk lost the exact prefix or SAS credential")
			}
		}
	}
	// A leading slash creates an empty component immediately below the
	// container, distinguishable from the container by its parent location.
	base = loc(t, "azure://account/c")
	remote.nodes = nil
	emptyComponent := add("", store.KindDir, 0)
	add("/inside", store.KindFile, 3)
	add("outside", store.KindFile, 100)
	for _, all := range []bool{false, true} {
		sizes, err := b.directorySizes(t.Context(), base, []entry{emptyComponent}, all, reporter(t))
		if err != nil || !slices.Equal(sizes, []directorySize{{3, 1}}) {
			t.Fatalf("leading slash, all=%v: %v, %v", all, sizes, err)
		}
	}
}

func TestDirectorySizesAtAccountLevel(t *testing.T) {
	b := testBackend(t)
	base := loc(t, "azure://account")
	remote := &sizeBlobs{}
	b.remote = remote
	var entries []entry
	for _, name := range []string{"one", "two", "empty"} {
		u := base.url.WithPathPart(name)
		entries = append(entries, entry{loc: location{url: u}, node: &store.Node{URL: u, Kind: store.KindDir}})
	}
	for _, name := range []string{"one/file", "one/sub/file", "two/file"} {
		remote.nodes = append(remote.nodes, &store.Node{URL: base.url.WithPathPart(name), Size: 3})
	}
	for _, all := range []bool{false, true} {
		sizes, err := b.directorySizes(t.Context(), base, entries, all, reporter(t))
		if err != nil || !slices.Equal(sizes, []directorySize{{6, 2}, {3, 1}, {}}) {
			t.Fatalf("all=%v: sizes=%v, err=%v", all, sizes, err)
		}
	}
}

func TestFailedDirectorySizeScansDiscardPartialTotals(t *testing.T) {
	for _, failure := range []string{"unreadable", "cancelled", "overflow"} {
		t.Run(failure, func(t *testing.T) {
			b := testBackend(t)
			root := t.TempDir()
			write(t, filepath.Join(root, "tree", "file"), "payload")
			entries := []entry{selected(t, b, filepath.Join(root, "tree"))}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			b.local = &sizeWalkStore{Store: b.local, scan: func(_ context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
				n := &store.Node{URL: u.Join("file"), Size: 7}
				if err := fn(n); err != nil {
					return err
				}
				switch failure {
				case "unreadable":
					return onError(u, errors.New("unreadable subtree"))
				case "cancelled":
					cancel()
					return nil // The caller must check cancellation after walking.
				default:
					n.Size = math.MaxInt64
					return fn(n)
				}
			}}
			sizes, err := b.directorySizes(ctx, loc(t, root), entries, false, reporter(t))
			if sizes != nil || err == nil || failure == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("partial result: %v, %v", sizes, err)
			}
		})
	}
}

func TestDirectorySizesAreOptInAndResortWithoutLosingSelection(t *testing.T) {
	a, screen, left, _ := testApp(t)
	write(t, filepath.Join(left, "a", "file"), "12345")
	write(t, filepath.Join(left, "b", "nested", "file"), "12")
	if err := os.Mkdir(filepath.Join(left, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	walk := &sizeWalkStore{Store: a.backend.local}
	a.backend.local = walk
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, 's')
	press(a, tcell.KeyRune, '3')
	press(a, tcell.KeyEnter, 0)
	if walk.walks != 0 {
		t.Fatal("browsing or sorting started a recursive scan")
	}
	p := &a.panes[0]
	marked, focused := p.entries[1].node, p.entries[2].node
	statSize := marked.Size
	press(a, tcell.KeyRune, ' ') // Mark a; cursor advances to b.
	press(a, tcell.KeyRune, 'z')
	finishJobs(t, a)
	if walk.walks != 1 || p.entries[1].dirSize == nil || p.entries[1].dirSize.bytes != 5 || p.entries[2].dirSize != nil {
		t.Fatal("z did not limit calculation to marked directories")
	}
	press(a, tcell.KeyRune, 'Z')
	finishJobs(t, a)
	if walk.walks != 2 || strings.Join(entryNames(p.entries), " ") != ".. empty b a" {
		t.Fatalf("Z did not calculate and sort every directory in one walk: %v", entryNames(p.entries))
	}
	if p.entries[p.cursor].node != focused || p.selection()[0].node != marked || marked.Size != statSize {
		t.Fatal("calculation lost focus/marks or overwrote stat metadata")
	}
	a.draw()
	if text := screenText(screen); !strings.Contains(text, "0 B") || !strings.Contains(text, "5 B") || !strings.Contains(text, "Size ↑") {
		t.Fatalf("calculated sizes or sort order not displayed:\n%s", text)
	}
	press(a, tcell.KeyCtrlR, 0)
	finishJobs(t, a)
	for _, e := range p.entries {
		if e.dirSize != nil || e.sizePending {
			t.Fatal("refresh retained stale calculated totals")
		}
	}
	if walk.walks != 2 {
		t.Fatal("refresh triggered a recursive scan")
	}
}

func TestDirectorySizeScanCanBeCancelledWithoutPublishingTotals(t *testing.T) {
	a, screen, left, _ := testApp(t)
	write(t, filepath.Join(left, "tree", "file"), "payload")
	a.reload()
	finishJobs(t, a)
	started := make(chan struct{})
	a.backend.local = &sizeWalkStore{Store: a.backend.local, scan: func(ctx context.Context, u *uri.URL, _ func(*uri.URL, error) error, fn func(*store.Node) error) error {
		if err := fn(&store.Node{URL: u.Join("file"), Size: 7}); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	press(a, tcell.KeyRune, 'z') // No marks: use the cursor.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("directory scan did not start")
	}
	a.draw()
	if text := screenText(screen); !strings.Contains(text, "7 B so far") || !strings.Contains(text, "Esc: cancel") || !strings.Contains(text, "…") || strings.Contains(text, "ETA") {
		t.Fatalf("scan progress is missing or presented as a transfer:\n%s", text)
	}
	press(a, tcell.KeyF8, 0)
	press(a, tcell.KeyRune, 's')
	if a.modal != nil {
		t.Fatal("sorting or deletion was allowed during a scan")
	}
	press(a, tcell.KeyEscape, 0)
	finishJobs(t, a)
	if e := a.panes[0].entries[1]; e.sizePending || e.dirSize != nil || !strings.HasPrefix(a.status, "Cancelled") {
		t.Fatal("cancelled scan left a pending or partially calculated total")
	}
	read(t, filepath.Join(left, "tree", "file"), "payload")
}

func TestFileCountColumnAndSortingReuseRecursiveScan(t *testing.T) {
	a, screen, left, _ := testApp(t)
	write(t, filepath.Join(left, "large", "file"), strings.Repeat("x", 100))
	write(t, filepath.Join(left, "many", "one"), "x")
	write(t, filepath.Join(left, "many", "nested", "two"), "y")
	walk := &sizeWalkStore{Store: a.backend.local}
	a.backend.local = walk
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, 's')
	press(a, tcell.KeyRune, '6')
	press(a, tcell.KeyRune, 'r')
	press(a, tcell.KeyEnter, 0)
	if !a.panes[0].showCounts || a.panes[1].showCounts || walk.walks != 0 {
		t.Fatal("selecting file-count sorting did not select the column, changed the other pane or started scanning")
	}
	press(a, tcell.KeyRune, 'Z')
	finishJobs(t, a)
	p := &a.panes[0]
	if strings.Join(entryNames(p.entries), " ") != ".. many large" || p.entries[1].dirSize.files != 2 || p.entries[2].dirSize.files != 1 {
		t.Fatal("file-count sort did not include nested files")
	}
	a.draw()
	// Inspect the active pane's count column, not numbers in the status area.
	column := func() string {
		var out strings.Builder
		for y := 3; y <= 6; y++ {
			for x := 38; x < 47; x++ {
				r, _, _, _ := screen.GetContent(x, y)
				out.WriteRune(r)
			}
			out.WriteByte('\n')
		}
		return out.String()
	}
	if got := column(); !strings.Contains(got, "Files") || !strings.Contains(got, "2") || !strings.Contains(got, "1") || strings.Contains(got, " B") {
		t.Fatalf("count column: %q", got)
	}
	press(a, tcell.KeyRune, 'f')
	a.draw()
	if got := column(); !strings.Contains(got, "Size") || !strings.Contains(got, "100 B") || !strings.Contains(got, "2 B") {
		t.Fatalf("size column: %q", got)
	}
	if walk.walks != 1 || strings.Join(entryNames(p.entries), " ") != ".. many large" || a.panes[1].showCounts {
		t.Fatal("toggling columns changed the sort, rescanned directories or changed the other pane")
	}
	press(a, tcell.KeyRune, 's')
	press(a, tcell.KeyRune, 'f')
	press(a, tcell.KeyEscape, 0)
	press(a, tcell.KeyCtrlR, 0)
	finishJobs(t, a)
	if !p.showCounts || p.sort.field != sortFiles || !p.sort.reverse || walk.walks != 1 {
		t.Fatal("refresh lost column/sort settings or triggered a recursive scan")
	}
}
