// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func testBackend(t testing.TB) *backend {
	t.Helper()
	o := cli.Defaults()
	o.Auth = azure.AuthAnonymous
	return newBackend(o, slog.New(slog.DiscardHandler))
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s: %q, %v; want %q", path, b, err, want)
	}
}

func loc(t testing.TB, path string) location {
	t.Helper()
	u, err := parseURL(path, "")
	if err != nil {
		t.Fatal(err)
	}
	return location{url: u}
}

func selected(t *testing.T, b *backend, path string) entry {
	t.Helper()
	l := loc(t, path)
	n, err := b.store(l.url).Stat(t.Context(), l.url, false)
	if err != nil {
		t.Fatal(err)
	}
	return entry{name: n.Name(), loc: l, node: n}
}

func reporter(t testing.TB) *progress.Reporter {
	t.Helper()
	p := progress.New(progress.Config{Mode: progress.ModeNever})
	t.Cleanup(p.Stop)
	return p
}

func TestLocalCopyMoveAndDelete(t *testing.T) {
	b := testBackend(t)
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "tree", "a[1]%.txt"), "literal name")
	if err := os.Mkdir(filepath.Join(left, "tree", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := selected(t, b, filepath.Join(left, "tree"))
	if err := b.transfer(t.Context(), []entry{src}, loc(t, right), false, reporter(t)); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(right, "tree", "a[1]%.txt"), "literal name")
	if fi, err := os.Stat(filepath.Join(right, "tree", "empty")); err != nil || !fi.IsDir() {
		t.Fatalf("empty directory: %v", err)
	}
	write(t, filepath.Join(left, "move-me"), "move contents")
	if err := b.transfer(t.Context(), []entry{selected(t, b, filepath.Join(left, "move-me"))}, loc(t, right), true, reporter(t)); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(right, "move-me"), "move contents")
	if _, err := os.Lstat(filepath.Join(left, "move-me")); !os.IsNotExist(err) {
		t.Fatalf("moved source still exists: %v", err)
	}
	if err := b.delete(t.Context(), []entry{selected(t, b, filepath.Join(right, "tree"))}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(right, "tree")); !os.IsNotExist(err) {
		t.Fatalf("deleted tree remains: %v", err)
	}
	read(t, filepath.Join(left, "tree", "a[1]%.txt"), "literal name")
}

func TestMoveFailureRetainsEverySource(t *testing.T) {
	b := testBackend(t)
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "good"), "keep me")
	write(t, filepath.Join(left, "tree", "child"), "also keep me")
	write(t, filepath.Join(right, "tree"), "blocks directory copy")
	sources := []entry{selected(t, b, filepath.Join(left, "good")), selected(t, b, filepath.Join(left, "tree"))}
	if err := b.transfer(t.Context(), sources, loc(t, right), true, reporter(t)); err == nil {
		t.Fatal("partial move reported success")
	}
	read(t, filepath.Join(left, "good"), "keep me")
	read(t, filepath.Join(left, "tree", "child"), "also keep me")
}

func TestMoveSkipDryRunAndCancellationRetainSources(t *testing.T) {
	for _, mode := range []string{"skip", "dry-run", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			b := testBackend(t)
			left, right := t.TempDir(), t.TempDir()
			write(t, filepath.Join(left, "a"), "original")
			src := selected(t, b, filepath.Join(left, "a"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "skip":
				b.opt.NoClobber = true
				write(t, filepath.Join(right, "a"), "existing")
			case "dry-run":
				b.opt.DryRun = true
			case "cancel":
				cancel()
			}
			err := b.transfer(ctx, []entry{src}, loc(t, right), true, reporter(t))
			if mode == "dry-run" && err != nil || mode != "dry-run" && err == nil {
				t.Fatalf("%s: %v", mode, err)
			}
			read(t, filepath.Join(left, "a"), "original")
		})
	}
}

type changedSource struct {
	store.Store
	calls  int
	change func()
}

func (s *changedSource) Stat(ctx context.Context, u *uri.URL, follow bool) (*store.Node, error) {
	s.calls++
	// Destination stat, initial inventory, then inventory after copying. The
	// copy engine uses its own local store, so this is deterministic.
	if s.calls == 3 {
		s.change()
	}
	return s.Store.Stat(ctx, u, follow)
}

func TestChangedSourceIsNotRemovedAfterCopy(t *testing.T) {
	b := testBackend(t)
	left, right := t.TempDir(), t.TempDir()
	path := filepath.Join(left, "a")
	write(t, path, "original")
	src := selected(t, b, path)
	b.local = &changedSource{Store: b.local, change: func() { write(t, path, "changed after copy") }}
	err := b.transfer(t.Context(), []entry{src}, loc(t, right), true, reporter(t))
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("changed source: %v", err)
	}
	read(t, path, "changed after copy")
	read(t, filepath.Join(right, "a"), "original")
}

type incompleteWalk struct{ store.Store }

func (s incompleteWalk) WalkAll(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
	return onError(u, errors.New("unreadable subtree"))
}

func TestDeletionWaitsForEveryInventory(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "first"), "keep first")
	write(t, filepath.Join(root, "tree", "child"), "keep child")
	items := []entry{selected(t, b, filepath.Join(root, "first")), selected(t, b, filepath.Join(root, "tree"))}
	b.local = incompleteWalk{b.local}
	if err := b.delete(t.Context(), items); err == nil {
		t.Fatal("partial listing permitted deletion")
	}
	read(t, filepath.Join(root, "first"), "keep first")
	read(t, filepath.Join(root, "tree", "child"), "keep child")
}

func TestRecursiveDeleteDoesNotFollowSymlinks(t *testing.T) {
	b := testBackend(t)
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "keep"), "outside")
	write(t, filepath.Join(root, "tree", "file"), "inside")
	link := filepath.Join(root, "tree", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := b.delete(t.Context(), []entry{selected(t, b, filepath.Join(root, "tree"))}); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(outside, "keep"), "outside")
}

func TestDeleteRefusesReplacedRoot(t *testing.T) {
	b := testBackend(t)
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "tree", "file"), "selected")
	write(t, filepath.Join(outside, "file"), "outside")
	trees, err := b.inventory(t.Context(), []entry{selected(t, b, filepath.Join(root, "tree"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "tree"), filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "tree")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := b.remove(t.Context(), trees); err == nil {
		t.Fatal("replaced root was removed")
	}
	read(t, filepath.Join(outside, "file"), "outside")
	read(t, filepath.Join(root, "saved", "file"), "selected")
}

func TestDeleteRefusesASwappedParentHoldingAHardLink(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "dir", "file"), "selected")
	trees, err := b.inventory(t.Context(), []entry{selected(t, b, filepath.Join(root, "dir", "file"))})
	if err != nil {
		t.Fatal(err)
	}
	// Another directory takes the path with a second link to the file, so
	// everything the file's own identity says still holds.
	if err := os.Rename(filepath.Join(root, "dir"), filepath.Join(root, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "saved", "file"), filepath.Join(root, "dir", "file")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := b.remove(t.Context(), trees); !errors.Is(err, errChanged) {
		t.Fatalf("removed through a directory that was not the one listed: %v", err)
	}
	read(t, filepath.Join(root, "dir", "file"), "selected")
}

func TestDeleteRefusesALookalikeSwappedIn(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	lookalike := func(path string) {
		t.Helper()
		write(t, path, "listed")
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ selected, swapped string }{
		{"top", "top"},
		{"tree", filepath.Join("tree", "child")},
	} {
		lookalike(filepath.Join(root, tc.swapped))
		trees, err := b.inventory(t.Context(), []entry{selected(t, b, filepath.Join(root, tc.selected))})
		if err != nil {
			t.Fatal(err)
		}
		// Alike in name, contents, size, time and mode: only which file it is
		// tells the two apart.
		if err := os.Rename(filepath.Join(root, tc.swapped), filepath.Join(root, tc.swapped+".listed")); err != nil {
			t.Fatal(err)
		}
		lookalike(filepath.Join(root, tc.swapped))
		if err := b.remove(t.Context(), trees); !errors.Is(err, errChanged) {
			t.Errorf("%s: removed a lookalike of what was listed: %v", tc.swapped, err)
		}
		read(t, filepath.Join(root, tc.swapped), "listed")
	}
}

func TestTheSlashEntryAtAContainerRootIsNotTheContainer(t *testing.T) {
	b := testBackend(t)
	account := loc(t, "azure://account.blob.core.windows.net")
	container := loc(t, "azure://account.blob.core.windows.net/container")
	dir := &store.Node{URL: container.url, Kind: store.KindDir}
	// What a listing of the container gives the names beginning with "/".
	slash := entry{name: "/", loc: location{url: container.url, parent: &container}, node: dir}
	for _, check := range []func(entry) error{removable, movable} {
		if err := check(slash); !errors.Is(err, errSharedURL) {
			t.Errorf("acted on the whole container: %v", err)
		}
	}
	if err := b.transfer(t.Context(), []entry{slash}, loc(t, t.TempDir()), false, reporter(t)); !errors.Is(err, errSharedURL) {
		t.Errorf("copied the whole container: %v", err)
	}
	whole := entry{name: "container", loc: location{url: container.url, parent: &account}, node: dir}
	if err := removable(whole); err != nil {
		t.Errorf("refused the container itself: %v", err)
	}
}

func TestCopyRefusesDestinationInsideSource(t *testing.T) {
	b := testBackend(t)
	root := t.TempDir()
	write(t, filepath.Join(root, "tree", "child", "file"), "keep")
	src := selected(t, b, filepath.Join(root, "tree"))
	if err := b.transfer(t.Context(), []entry{src}, loc(t, filepath.Join(root, "tree", "child")), true, reporter(t)); err == nil {
		t.Fatal("move into itself succeeded")
	}
	read(t, filepath.Join(root, "tree", "child", "file"), "keep")
}

func TestPreviewIsBoundedAndReadOnly(t *testing.T) {
	b := testBackend(t)
	path := filepath.Join(t.TempDir(), "big")
	data := strings.Repeat("x", 70<<10)
	write(t, path, data)
	preview, err := b.preview(t.Context(), selected(t, b, path))
	if err != nil || !strings.HasSuffix(preview, "[Preview limited to 64 KiB]") || len(preview) > 66000 {
		t.Fatalf("preview length %d: %v", len(preview), err)
	}
	read(t, path, data)
	write(t, path, "binary\x00data")
	preview, err = b.preview(t.Context(), selected(t, b, path))
	if err != nil || !strings.Contains(preview, "Binary file") {
		t.Fatalf("binary preview: %q, %v", preview, err)
	}
}

func TestDryRunDeleteAndMkdirDoNotMutate(t *testing.T) {
	b := testBackend(t)
	b.opt.DryRun = true
	root := t.TempDir()
	write(t, filepath.Join(root, "file"), "keep")
	if err := b.delete(t.Context(), []entry{selected(t, b, filepath.Join(root, "file"))}); err != nil {
		t.Fatal(err)
	}
	if err := b.mkdir(t.Context(), loc(t, root), "new"); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(root, "file"), "keep")
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created directory: %v", err)
	}
}

func TestContainersCanBeDeletedButNotMoved(t *testing.T) {
	dir := &store.Node{Kind: store.KindDir}
	for _, tc := range []struct {
		path         string
		remove, move bool
	}{
		{"azure://account.blob.core.windows.net", false, false},
		{"azure://account.blob.core.windows.net/container", true, false},
		{"azure://account.blob.core.windows.net/container/prefix", true, true},
		{"/", false, false},
	} {
		e := entry{loc: loc(t, tc.path), node: dir}
		if got := removable(e) == nil; got != tc.remove {
			t.Errorf("%s removable = %v", tc.path, got)
		}
		if got := movable(e) == nil; got != tc.move {
			t.Errorf("%s movable = %v", tc.path, got)
		}
	}
}
