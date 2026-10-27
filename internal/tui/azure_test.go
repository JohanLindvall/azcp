// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
)

// scripts/e2e.sh supplies a disposable emulator. Ordinary tests use no account,
// network endpoint or credential from the developer's environment.
func TestAzuriteCommanderOperations(t *testing.T) {
	endpoint := os.Getenv("AZCP_TUI_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("set AZCP_TUI_TEST_ENDPOINT to the disposable Azurite account URL")
	}
	t.Setenv("AZURE_CONFIG_DIR", t.TempDir())
	b := testBackend(t)
	o := b.opt
	o.Auth, o.CreateContainer = azure.AuthAuto, true
	b = newBackend(&o, slog.New(slog.DiscardHandler))
	container := fmt.Sprintf("tui-%d", time.Now().UnixNano())
	remote := loc(t, endpoint+"/"+container)
	if err := b.az.MkdirAll(t.Context(), remote.url, 0); err != nil {
		t.Fatal(err)
	}
	key, err := azblob.NewSharedKeyCredential(os.Getenv("AZURE_STORAGE_ACCOUNT"), os.Getenv("AZURE_STORAGE_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := azblob.NewClientWithSharedKeyCredential(endpoint, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := client.DeleteContainer(ctx, container, nil); err != nil {
			t.Errorf("clean up test container: %v", err)
		}
	})
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "tree", "a[1]%.txt"), "blob contents")
	write(t, filepath.Join(left, "tree", "sub", "deep", "leaf.txt"), "nested contents")
	if err := os.Mkdir(filepath.Join(left, "tree", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.transfer(t.Context(), []entry{selected(t, b, filepath.Join(left, "tree"))}, remote, false, reporter(t)); err != nil {
		t.Fatal(err)
	}
	entries, err := b.list(t.Context(), remote)
	if err != nil || len(entries) != 1 || entries[0].name != "tree" || !entries[0].directory() {
		t.Fatalf("container listing: %+v, %v", entries, err)
	}
	tree := entries[0]
	children, err := b.list(t.Context(), tree.loc)
	if err != nil || len(children) != 3 {
		t.Fatalf("virtual prefix listing: %v, %v", children, err)
	}
	usage, err := b.scanUsage(t.Context(), remote, false, reporter(t))
	if err != nil || usage.root.total.logical != 28 || usage.root.total.files != 2 || usage.root.total.incomplete {
		t.Fatalf("Azure usage scan: %+v, %v", usage, err)
	}
	usageTree := findUsage(t, usage.root, "tree", store.KindDir)
	if !findUsage(t, usageTree, "empty", store.KindDir).marker {
		t.Fatal("usage cache lost the empty directory marker")
	}
	for _, all := range []bool{false, true} {
		trees, err := b.directorySizes(t.Context(), remote, []entry{tree}, all, reporter(t))
		sizes := sizeTotals(trees)
		if err != nil || len(sizes) != 1 || sizes[0] != (directorySize{28, 2}) {
			t.Fatalf("remote tree size, all=%v: %v, %v", all, sizes, err)
		}
		trees, err = b.directorySizes(t.Context(), tree.loc, children[:2], all, reporter(t))
		sizes = sizeTotals(trees)
		if err != nil || len(sizes) != 2 || sizes[0] != (directorySize{}) || sizes[1] != (directorySize{15, 1}) {
			t.Fatalf("remote child sizes, all=%v: %v, %v", all, sizes, err)
		}
	}
	accountEntries, err := b.list(t.Context(), loc(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	foundContainer := false
	for _, e := range accountEntries {
		if e.name == container && e.directory() {
			foundContainer = true
		}
	}
	if !foundContainer {
		t.Fatal("account did not expose the container as a directory")
	}
	deeper, err := b.list(t.Context(), children[1].loc)
	if err != nil || len(deeper) != 1 || deeper[0].name != "deep" || !deeper[0].directory() {
		t.Fatalf("nested prefix: %+v, %v", deeper, err)
	}
	if err := b.mkdir(t.Context(), deeper[0].loc, "new-empty"); err != nil {
		t.Fatal(err)
	}
	file := children[2] // directories first
	preview, err := b.preview(t.Context(), file)
	if err != nil || preview != "blob contents" {
		t.Fatalf("remote preview: %q, %v", preview, err)
	}
	if err := b.transfer(t.Context(), []entry{tree}, loc(t, right), false, reporter(t)); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(right, "tree", "a[1]%.txt"), "blob contents")
	read(t, filepath.Join(right, "tree", "sub", "deep", "leaf.txt"), "nested contents")
	if fi, err := os.Stat(filepath.Join(right, "tree", "sub", "deep", "new-empty")); err != nil || !fi.IsDir() {
		t.Fatalf("nested empty directory: %v", err)
	}
	if err := b.mkdir(t.Context(), remote, "moved"); err != nil {
		t.Fatal(err)
	}
	if err := b.transfer(t.Context(), []entry{file}, loc(t, endpoint+"/"+container+"/moved/"), true, reporter(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.az.Stat(t.Context(), file.loc.url, false); !store.IsNotExist(err) {
		t.Fatalf("remote move left source: %v", err)
	}
	if err := b.transfer(t.Context(), []entry{children[1]}, loc(t, endpoint+"/"+container+"/moved/"), true, reporter(t)); err != nil {
		t.Fatalf("move nested virtual directory: %v", err)
	}
	// A blob can share a prefix's unsuffixed name. Removing the prefix must
	// leave that independent blob alone, even when the prefix has no marker.
	sibling := filepath.Join(left, "sibling")
	write(t, sibling, "same-name blob stays")
	if err := b.az.Upload(t.Context(), sibling, tree.loc.url, azure.TransferOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err = b.list(t.Context(), remote)
	if err != nil {
		t.Fatal(err)
	}
	files, dirs := 0, 0
	for _, e := range entries {
		if e.name == "tree" {
			if e.directory() {
				dirs++
			} else {
				files++
			}
		}
	}
	if files != 1 || dirs != 1 {
		t.Fatalf("filesystem view hid a colliding blob or prefix: %d files, %d directories", files, dirs)
	}
	for _, all := range []bool{false, true} {
		trees, err := b.directorySizes(t.Context(), remote, []entry{tree}, all, reporter(t))
		sizes := sizeTotals(trees)
		if err != nil || len(sizes) != 1 || sizes[0] != (directorySize{}) {
			t.Fatalf("empty prefix counted the independent blob, all=%v: %v, %v", all, sizes, err)
		}
	}
	if err := b.delete(t.Context(), []entry{tree}); err != nil {
		t.Fatal(err)
	}
	n, err := b.az.Stat(t.Context(), tree.loc.url, false)
	if err != nil || !n.IsRegular() {
		t.Fatalf("prefix deletion lost independent blob: %v", err)
	}
	preview, err = b.preview(t.Context(), entry{loc: tree.loc, node: n})
	if err != nil || !strings.Contains(preview, "same-name blob stays") {
		t.Fatalf("independent blob: %q, %v", preview, err)
	}
	// Move a whole remote tree back to local storage, including its marker.
	moved := selected(t, b, endpoint+"/"+container+"/moved/")
	if err := b.transfer(t.Context(), []entry{moved}, loc(t, right), true, reporter(t)); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(right, "moved", "a[1]%.txt"), "blob contents")
	read(t, filepath.Join(right, "moved", "sub", "deep", "leaf.txt"), "nested contents")
	if _, err := b.az.Stat(t.Context(), nodeURL(moved.node), false); !store.IsNotExist(err) {
		t.Fatalf("moved prefix remains: %v", err)
	}
	// Removing the last file prunes a virtual prefix, but a real empty marker
	// survives. The incrementally updated cache must match a fresh flat scan.
	if err := b.mkdir(t.Context(), remote, "usage-marked"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"usage-marked", "usage-virtual"} {
		if err := b.az.Upload(t.Context(), sibling, remote.url.Join(name, "file"), azure.TransferOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	usage, err = b.scanUsage(t.Context(), remote, false, reporter(t))
	if err != nil {
		t.Fatal(err)
	}
	var removed []entry
	for _, name := range []string{"usage-marked", "usage-virtual"} {
		d := findUsage(t, usage.root, name, store.KindDir)
		removed = append(removed, usage.entry(findUsage(t, d, "file", store.KindFile)))
	}
	if err := b.delete(t.Context(), removed); err != nil {
		t.Fatal(err)
	}
	updated, err := b.usageAfterDelete(t.Context(), usage, removed)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := b.scanUsage(t.Context(), remote, false, reporter(t))
	if err != nil {
		t.Fatal(err)
	}
	if updated.root.total != fresh.root.total || len(updated.root.children) != len(fresh.root.children) {
		t.Fatalf("cached cleanup differs from storage: updated=%+v fresh=%+v err=%v", updated.root.total, fresh.root.total, err)
	}
}
