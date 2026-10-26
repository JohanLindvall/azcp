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
	var err error
	b, err = newBackend(&o, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil || len(children) != 2 {
		t.Fatalf("virtual prefix listing: %v, %v", children, err)
	}
	file := children[1] // directories first
	preview, err := b.preview(t.Context(), file)
	if err != nil || preview != "blob contents" {
		t.Fatalf("remote preview: %q, %v", preview, err)
	}
	if err := b.transfer(t.Context(), []entry{tree}, loc(t, right), false, reporter(t)); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(right, "tree", "a[1]%.txt"), "blob contents")
	if err := b.mkdir(t.Context(), remote, "moved"); err != nil {
		t.Fatal(err)
	}
	if err := b.transfer(t.Context(), []entry{file}, loc(t, endpoint+"/"+container+"/moved/"), true, reporter(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.az.Stat(t.Context(), file.loc.url, false); !store.IsNotExist(err) {
		t.Fatalf("remote move left source: %v", err)
	}
	// A blob can share a prefix's unsuffixed name. Removing the prefix must
	// leave that independent blob alone, even when the prefix has no marker.
	sibling := filepath.Join(left, "sibling")
	write(t, sibling, "same-name blob stays")
	if err := b.az.Upload(t.Context(), sibling, tree.loc.url, azure.TransferOptions{}); err != nil {
		t.Fatal(err)
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
	if _, err := b.az.Stat(t.Context(), nodeURL(moved.node), false); !store.IsNotExist(err) {
		t.Fatalf("moved prefix remains: %v", err)
	}
}
