// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func findUsage(t *testing.T, n *usageNode, name string, kind store.Kind) *usageNode {
	t.Helper()
	for _, child := range n.children {
		if child.name == name && child.kind == kind {
			return child
		}
	}
	t.Fatalf("missing %s %q", kind, name)
	return nil
}

func TestUsageTreeCountsHardLinksOncePerAncestorAndKeepsBothNames(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "one"), "payload")
	if err := os.Mkdir(filepath.Join(root, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a/two", "b/three"} {
		if err := os.Link(filepath.Join(root, "a", "one"), filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
	}
	tree, err := b.scanUsage(t.Context(), loc(t, root), false, reporter(t))
	if err != nil {
		t.Fatal(err)
	}
	a, c := findUsage(t, tree.root, "a", store.KindDir), findUsage(t, tree.root, "b", store.KindDir)
	file := findUsage(t, a, "one", store.KindFile)
	if len(a.children) != 2 || file.link == nil || a.total.files != 2 || c.total.files != 1 || tree.root.total.files != 3 {
		t.Fatal("hard-link names were lost or miscounted")
	}
	if a.total.logical != a.length+7 || c.total.logical != c.length+7 || tree.root.total.logical != tree.root.length+a.length+c.length+7 {
		t.Fatalf("hard-linked data counted twice: a=%+v b=%+v root=%+v", a.total, c.total, tree.root.total)
	}
	if tree.root.total.disk != tree.root.allocated+a.allocated+c.allocated+file.allocated || tree.root.total.diskUnknown || tree.root.total.incomplete {
		t.Fatalf("allocated totals: %+v", tree.root.total)
	}
	selected := []entry{tree.entry(file)}
	if err := b.delete(t.Context(), selected); err != nil {
		t.Fatal(err)
	}
	updated, err := b.usageAfterDelete(t.Context(), tree, selected)
	if err != nil {
		t.Fatal(err)
	}
	newA, newB := findUsage(t, updated.root, "a", store.KindDir), findUsage(t, updated.root, "b", store.KindDir)
	if updated.root.total.files != 2 || updated.root.total.logical != updated.root.length+newA.length+newB.length+7 {
		t.Fatalf("deleting one alias subtracted shared bytes: %+v", updated.root.total)
	}
	if len(a.children) != 2 || tree.root.total.files != 3 || file.link.count != 3 {
		t.Fatal("updating the cache mutated the displayed snapshot")
	}
}

type usageReadStore struct {
	store.Store
	reads  int
	failed string
}

func (s *usageReadStore) ReadDir(ctx context.Context, u *uri.URL) ([]*store.Node, error) {
	s.reads++
	if filepath.Base(u.Path) == s.failed {
		return nil, errors.New("permission denied")
	}
	return s.Store.ReadDir(ctx, u)
}

func TestUsageContinuesPastErrorsAndMarksAncestorsIncomplete(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "bad", "hidden"), "hidden")
	write(t, filepath.Join(root, "good", "file"), "good")
	b.local = &usageReadStore{Store: b.local, failed: "bad"}
	tree, err := b.scanUsage(t.Context(), loc(t, root), false, reporter(t))
	if err != nil || tree.issueCount != 1 || !tree.root.total.incomplete {
		t.Fatalf("scan errors: %v, %v", tree, err)
	}
	bad, good := findUsage(t, tree.root, "bad", store.KindDir), findUsage(t, tree.root, "good", store.KindDir)
	if !bad.total.incomplete || good.total.incomplete || good.total.files != 1 || len(good.children) != 1 {
		t.Fatal("unreadable subtree stopped its readable sibling")
	}
	if _, known := tree.entry(bad).size(); known {
		t.Fatal("incomplete size was presented as complete")
	}
}

func TestUsageOneFilesystemSkipsMountsWithoutReadingThem(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "mount", "file"), "not scanned")
	write(t, filepath.Join(root, "local", "file"), "scanned")
	original := b.usageOf
	b.usageOf = func(path string, info fs.FileInfo) (local.Usage, error) {
		u, err := original(path, info)
		if filepath.Base(path) == "mount" {
			u.ID.Dev++
		}
		return u, err
	}
	walker := &usageReadStore{Store: b.local}
	b.local = walker
	tree, err := b.scanUsage(t.Context(), loc(t, root), true, reporter(t))
	if err != nil || walker.reads != 2 || tree.root.total.excluded != 1 || tree.root.total.files != 1 {
		t.Fatalf("filesystem boundary: reads=%d tree=%+v err=%v", walker.reads, tree, err)
	}
	if !findUsage(t, tree.root, "mount", store.KindDir).excluded {
		t.Fatal("mount not marked excluded")
	}
}

type usageBlobs struct{ *sizeBlobs }

func (s usageBlobs) Stat(_ context.Context, u *uri.URL, _ bool) (*store.Node, error) {
	return &store.Node{URL: u, Kind: store.KindDir}, nil
}

func TestUsageAzureTreeKeepsExactNamesAndCachesOneFlatWalk(t *testing.T) {
	b := testBackend(t)
	base := loc(t, "azure://account/c/prefix?sig=SECRET")
	remote := usageBlobs{&sizeBlobs{}}
	b.remote = remote
	for _, key := range []string{"prefix/a", "prefix/a/file", "prefix//file", "prefix/./file", "prefix/../file"} {
		remote.nodes = append(remote.nodes, &store.Node{URL: base.url.WithPathPart("c/" + key), Size: 5})
	}
	tree, err := b.scanUsage(t.Context(), base, false, reporter(t))
	if err != nil || len(remote.walks) != 1 || tree.root.total.logical != 25 || tree.root.total.files != 5 {
		t.Fatalf("flat scan: %+v, %v", tree, err)
	}
	findUsage(t, tree.root, "a", store.KindFile)
	for _, name := range []string{"a", "", ".", ".."} {
		d := findUsage(t, tree.root, name, store.KindDir)
		f := findUsage(t, d, "file", store.KindFile)
		l := tree.location(f)
		if l.url.Key != "prefix/"+name+"/file" || l.url.SAS != base.url.SAS || d.total.logical != 5 {
			t.Fatalf("lost exact key or credential: %q", l.url.Key)
		}
	}
}

func TestUsagePrunesEmptyVirtualPrefixesButKeepsMarkers(t *testing.T) {
	b := testBackend(t)
	base := loc(t, "azure://account/c")
	remote := usageBlobs{&sizeBlobs{}}
	b.remote = remote
	remote.nodes = []*store.Node{
		{URL: base.url.WithPathPart("c/virtual/nested/file"), Size: 7},
		{URL: base.url.WithPathPart("c/marked"), Kind: store.KindDir, ETag: "marker"},
		{URL: base.url.WithPathPart("c/marked/file"), Size: 9},
	}
	tree, err := b.scanUsage(t.Context(), base, false, reporter(t))
	if err != nil {
		t.Fatal(err)
	}
	virtual := findUsage(t, tree.root, "virtual", store.KindDir)
	marked := findUsage(t, tree.root, "marked", store.KindDir)
	selected := []entry{
		tree.entry(findUsage(t, findUsage(t, virtual, "nested", store.KindDir), "file", store.KindFile)),
		tree.entry(findUsage(t, marked, "file", store.KindFile)),
	}
	updated, err := tree.without(t.Context(), selected)
	if err != nil || len(updated.root.children) != 1 || updated.root.children[0].name != "marked" || updated.root.total.files != 0 {
		t.Fatalf("virtual prefix cleanup: %+v, %v", updated, err)
	}
	if len(tree.root.children) != 2 || tree.root.total.files != 2 {
		t.Fatal("original snapshot was mutated")
	}
}

func TestUsageDoesNotFollowSymlinkTrees(t *testing.T) {
	b := testBackend(t)
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "inside"), "inside")
	write(t, filepath.Join(outside, "outside"), "outside")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	tree, err := b.scanUsage(t.Context(), loc(t, root), false, reporter(t))
	if err != nil || tree.root.total.files != 1 || len(tree.root.children) != 3 {
		t.Fatalf("followed symlink: %+v, %v", tree, err)
	}
}

type usageBlockedRead struct {
	store.Store
	started chan struct{}
}

func (s usageBlockedRead) ReadDir(ctx context.Context, _ *uri.URL) ([]*store.Node, error) {
	close(s.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCancelledUsageScanLeavesTheOriginalPaneAndJoinsWorker(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "file"), "keep")
	a.reload()
	finishJobs(t, a)
	original := a.panes[0].entries[1].node
	started := make(chan struct{})
	a.backend.local = usageBlockedRead{a.backend.local, started}
	press(a, tcell.KeyRune, 'u')
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not start")
	}
	press(a, tcell.KeyEscape, 0)
	finishJobs(t, a)
	if a.panes[0].usage != nil || a.panes[0].entries[1].node != original || !strings.HasPrefix(a.status, "Cancelled") {
		t.Fatal("cancelled scan published partial state")
	}
	a.workers.Wait()
}

func TestUsageCannotDeleteAnIncompleteTreeAndFilterDoesNotChangeTotals(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "bad", "hidden"), "hidden")
	write(t, filepath.Join(left, "good", "file"), "good")
	a.backend.local = &usageReadStore{Store: a.backend.local, failed: "bad"}
	press(a, tcell.KeyRune, 'u')
	finishJobs(t, a)
	p := &a.panes[0]
	for i, e := range p.entries {
		if e.name == "bad" {
			p.cursor = i
		}
	}
	press(a, tcell.KeyF8, 0)
	if a.modal != nil || a.busy != nil || !strings.Contains(a.status, "unreadable or excluded") {
		t.Fatal("incomplete subtree was deletable")
	}
	before := p.usage.root.total
	press(a, tcell.KeyRune, ' ')
	press(a, tcell.KeyRune, '/')
	for _, r := range "good" {
		press(a, tcell.KeyRune, r)
	}
	press(a, tcell.KeyEnter, 0)
	if len(p.entries) != 2 || p.entries[1].name != "good" || len(p.selected) != 0 || p.usage.root.total != before {
		t.Fatal("filter changed totals, retained hidden marks or lost its match")
	}
}

func TestUsageConfirmedDeletionUpdatesTheCacheWithoutRescanning(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "remove"), "payload")
	write(t, filepath.Join(left, "keep", "file"), "keep")
	reads := &usageReadStore{Store: a.backend.local}
	a.backend.local = reads
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, 'u')
	finishJobs(t, a)
	p := &a.panes[0]
	for i, e := range p.entries {
		if e.name == "remove" {
			p.cursor = i
		}
	}
	before := reads.reads
	press(a, tcell.KeyF8, 0)
	press(a, tcell.KeyRune, 'y')
	finishJobs(t, a)
	if p.usage == nil || p.usage.root.total.files != 1 || len(p.usage.root.children) != 1 || reads.reads != before+1 {
		t.Fatalf("deletion rescanned the tree or left stale totals: reads %d -> %d", before, reads.reads)
	}
	if _, err := os.Stat(filepath.Join(left, "remove")); !os.IsNotExist(err) {
		t.Fatalf("deletion did not remove the selection: %v", err)
	}
	read(t, filepath.Join(left, "keep", "file"), "keep")
}

func TestUsageCleanupDiscardsCacheWhenTheParentIsReplaced(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "file"), "payload")
	tree, err := b.scanUsage(t.Context(), loc(t, root), false, reporter(t))
	if err != nil {
		t.Fatal(err)
	}
	selected := []entry{tree.entry(findUsage(t, tree.root, "file", store.KindFile))}
	if err := b.delete(t.Context(), selected); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(t.TempDir(), "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	updated, err := b.usageAfterDelete(t.Context(), tree, selected)
	if updated != nil || err == nil || !strings.Contains(err.Error(), "parent directory changed") {
		t.Fatalf("retained cache beneath a replaced parent: %+v, %v", updated, err)
	}
}

func TestUsageViewDrillsDownWithoutIOAndKeepsConfirmedDeletion(t *testing.T) {
	a, screen, left, _ := testApp(t)
	write(t, filepath.Join(left, "tree", "nested", "file.txt"), "payload")
	write(t, filepath.Join(left, "plain"), "x")
	reads := &usageReadStore{Store: a.backend.local}
	a.backend.local = reads
	a.reload()
	finishJobs(t, a)
	press(a, tcell.KeyRune, 'u')
	finishJobs(t, a)
	p := &a.panes[0]
	if p.usage == nil || p.sort.field != sortSize || !p.sort.reverse || !p.sort.mixDirectories {
		t.Fatal("usage view did not start largest-first")
	}
	for _, e := range p.entries[1:] {
		if e.node != nil || e.loc.url != nil {
			t.Fatal("cached rows retained full metadata nodes or URLs")
		}
	}
	afterScan := reads.reads
	for i, e := range p.entries {
		if e.name == "tree" {
			p.cursor = i
		}
	}
	press(a, tcell.KeyEnter, 0)
	press(a, tcell.KeyEnter, 0)
	if p.usageDir.name != "nested" || a.busy != nil || reads.reads != afterScan {
		t.Fatal("drill-down rescanned or lost its location")
	}
	press(a, tcell.KeyLeft, 0)
	if p.entries[p.cursor].name != "nested" || reads.reads != afterScan {
		t.Fatal("going up did not preserve focus or used storage")
	}
	press(a, tcell.KeyRune, 'i')
	if a.modal == nil || a.modal.title != "Usage details" {
		t.Fatal("usage details did not open")
	}
	press(a, tcell.KeyEscape, 0)
	for _, size := range [][2]int{{50, 14}, {100, 30}, {140, 40}} {
		screen.SetSize(size[0], size[1])
		a.draw()
		if text := screenText(screen); !strings.Contains(text, "USAGE") || !strings.Contains(text, "Files:") {
			t.Fatalf("usage rendering at %v:\n%s", size, text)
		}
	}
	press(a, tcell.KeyF8, 0)
	if a.modal == nil || a.modal.kind != "confirm" || a.busy != nil {
		t.Fatal("usage deletion did not require confirmation")
	}
	press(a, tcell.KeyEscape, 0)
	read(t, filepath.Join(left, "tree", "nested", "file.txt"), "payload")
	press(a, tcell.KeyRune, 'u')
	finishJobs(t, a)
	if p.usage != nil || p.sort != (paneSort{}) {
		t.Fatal("return to two panes lost the original sort settings")
	}
}
