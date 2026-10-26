// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestVirtualContainersResolveAndNavigate(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "tui.json")
	write(t, file, `{"containers":{"work":{"local":"files","cloud":"azure://account/container/prefix?sig=SECRET"}}}`)
	c, err := loadCollections(file, "")
	if err != nil {
		t.Fatal(err)
	}
	index, err := c.resolve("virtual://", "")
	if err != nil || len(c.list(index)) != 1 {
		t.Fatalf("index: %v", err)
	}
	l, err := c.resolve("virtual://work/local/sub", "")
	if err != nil || l.url.Path != filepath.ToSlash(filepath.Join(root, "files", "sub")) {
		t.Fatalf("mounted path: %+v, %v", l, err)
	}
	for _, want := range []string{"virtual://work/local", "virtual://work", "virtual://", "virtual://"} {
		l = c.parent(l, "")
		if l.display() != want {
			t.Fatalf("parent: %s, want %s", l.display(), want)
		}
	}
	cloud, err := c.resolve("virtual://work/cloud/sub", "")
	if err != nil || cloud.url.SAS != "sig=SECRET" || cloud.url.Key != "prefix/sub" {
		t.Fatalf("cloud mount lost credentials or path: %v", err)
	}
	if strings.Contains(cloud.display(), "SECRET") {
		t.Fatal("virtual path exposed credentials")
	}
	for _, raw := range []string{"virtual://missing", "virtual://work/missing", "virtual://work/local/..", "virtual://work/local/%2e%2e", "virtual://work?sig=x"} {
		if _, err := c.resolve(raw, ""); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestContainersCanContainContainers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tui.json")
	write(t, file, `{"containers":{"project":{"environments":{"production":{"blobs":"azure://account/c/prefix?sig=SECRET","empty":{}},"development":{"files":"workspace"}},"checkout":"src"}}}`)
	c, err := loadCollections(file, "")
	if err != nil {
		t.Fatal(err)
	}
	group, err := c.resolve("virtual://project/environments/production", "")
	if err != nil || group.url != nil {
		t.Fatalf("nested container: %+v, %v", group, err)
	}
	entries := c.list(group)
	if len(entries) != 2 || entries[0].name != "blobs" || entries[1].name != "empty" || entries[1].node != nil {
		t.Fatalf("nested entries: %+v", entries)
	}
	l, err := c.resolve("virtual://project/environments/production/blobs/sub", "")
	if err != nil || l.url.Key != "prefix/sub" || l.url.SAS != "sig=SECRET" {
		t.Fatalf("nested mount: %+v, %v", l, err)
	}
	for _, want := range []string{
		"virtual://project/environments/production/blobs",
		"virtual://project/environments/production", "virtual://project/environments",
		"virtual://project", "virtual://",
	} {
		l = c.parent(l, "")
		if l.display() != want {
			t.Fatalf("nested parent: %q, want %q", l.display(), want)
		}
	}
	empty := entries[1].loc
	if len(c.list(empty)) != 0 || c.parent(empty, "").display() != group.display() {
		t.Fatal("empty nested container lost its parent")
	}
	local, err := c.resolve("virtual://project/environments/development/files", "")
	if err != nil || local.url.Path != filepath.ToSlash(filepath.Join(filepath.Dir(file), "workspace")) {
		t.Fatalf("nested local mount: %+v, %v", local, err)
	}
}

func TestVirtualDestinationCopiesToItsBackingDirectory(t *testing.T) {
	b := testBackend(t)
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "file"), "via virtual container")
	b.collections["work"] = &virtualNode{children: collections{"destination": {url: loc(t, right).url}}}
	target, err := b.collections.resolve("virtual://work/destination", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.transfer(t.Context(), []entry{selected(t, b, filepath.Join(left, "file"))}, target, false, reporter(t)); err != nil {
		t.Fatal(err)
	}
	read(t, filepath.Join(right, "file"), "via virtual container")
	root, _ := b.collections.resolve("virtual://work", "")
	for _, e := range b.collections.list(root) {
		if err := b.delete(t.Context(), []entry{e}); err == nil {
			t.Fatal("removed a virtual reference")
		}
	}
}

func TestConfigErrorsAreNotSilentlyIgnored(t *testing.T) {
	for _, text := range []string{`{`, `{"container":{}}`, `{"containers":{"../x":{}}}`, `{"containers":{"x":{"y":"virtual://x"}}}`, `{} {}`} {
		file := filepath.Join(t.TempDir(), "tui.json")
		write(t, file, text)
		if _, err := loadCollections(file, ""); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
	if _, err := loadCollections(filepath.Join(t.TempDir(), "absent"), ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit missing config: %v", err)
	}
}

func TestEngineOperandsRoundTripExactBlobNamesAndSAS(t *testing.T) {
	for _, raw := range []string{
		"azure://account/container/a%3Fb%23c%25%5B1%5D?sv=1&sig=SECRET",
		"http://127.0.0.1:10000/devstoreaccount1/c/a%2F%2Fb%2F?sig=SECRET",
		"azure://account/c/%2Fleading%2F%2Fslashes",
	} {
		for _, dir := range []bool{false, true} {
			u, err := uri.Parse(raw, uri.Options{})
			if err != nil {
				t.Fatal(err)
			}
			v, err := uri.Parse(operand(u, dir), uri.Options{})
			if err != nil || u.Host != v.Host || u.Account != v.Account || u.Container != v.Container || u.Key != v.Key || u.SAS != v.SAS || v.TrailingSlash != dir || u.Secure != v.Secure {
				t.Fatalf("operand did not round-trip: %s; %v", u.Display(), err)
			}
		}
	}
}

type listedBlobs struct {
	blobStore
	nodes []*store.Node
}

func (s listedBlobs) ReadDir(context.Context, *uri.URL) ([]*store.Node, error) {
	return s.nodes, nil
}

func TestVirtualNavigationKeepsExactRemoteParents(t *testing.T) {
	b := testBackend(t)
	base := loc(t, "azure://account/c/prefix")
	base.address = "virtual://work/cloud"
	for _, key := range []string{"prefix/", "prefix/.", "prefix/.."} {
		u := base.url.WithPathPart("c/" + key)
		b.remote = listedBlobs{blobStore: b.remote, nodes: []*store.Node{{URL: u, Kind: store.KindDir}}}
		entries, err := b.list(t.Context(), base)
		if err != nil || len(entries) != 1 || entries[0].loc.url.Key != key {
			t.Fatalf("listed %q: %v, %v", key, entries, err)
		}
		parent := b.collections.parent(entries[0].loc, "")
		if parent.display() != base.display() || parent.url.Key != base.url.Key {
			t.Fatalf("lost exact parent of %q: %+v", key, parent)
		}
	}
}
