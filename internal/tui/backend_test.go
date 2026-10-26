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

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func testBackend(t *testing.T) *backend {
	t.Helper()
	o := cli.Defaults()
	o.TUIConfig = filepath.Join(t.TempDir(), "tui.json")
	write(t, o.TUIConfig, `{ "containers": {} }`)
	o.Auth = azure.AuthAnonymous
	b, err := newBackend(o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return b
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

func loc(t *testing.T, path string) location {
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

func reporter(t *testing.T) *progress.Reporter {
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
