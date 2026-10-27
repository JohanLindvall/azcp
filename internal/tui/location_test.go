// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestLocalRootAndParentStayOnTheirVolume(t *testing.T) {
	l := loc(t, t.TempDir())
	root := rootLocation(l)
	if !filepath.IsAbs(root.url.Path) || filepath.VolumeName(root.url.Path) != filepath.VolumeName(l.url.Path) {
		t.Fatalf("wrong filesystem root: %s", root.display())
	}
	if parentLocation(root).url.Path != root.url.Path {
		t.Fatalf("root has a different parent: %s", parentLocation(root).display())
	}
}

func TestAzureHierarchyBehavesLikeDirectories(t *testing.T) {
	b := testBackend(t)
	l := loc(t, "azure://account/container/first/second/third?sig=SECRET")
	current, err := b.resolve(".", l)
	if err != nil || current.url.Key != l.url.Key {
		t.Fatalf("relative current directory: %v", err)
	}
	for _, want := range []string{"container/first/second", "container/first", "container", "", ""} {
		l = parentLocation(l)
		if l.url.PathPart() != want || l.url.SAS != "sig=SECRET" {
			t.Fatalf("parent path = %q, want %q", l.url.PathPart(), want)
		}
	}
	l = rootLocation(loc(t, "azure://account/container/first/second?sig=SECRET"))
	if l.url.Container != "container" || l.url.Key != "" || l.url.SAS != "sig=SECRET" {
		t.Fatalf("container root lost its scope: %s", l.display())
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

func (s listedBlobs) ReadDirEntries(context.Context, *uri.URL) ([]*store.Node, error) {
	return s.nodes, nil
}

func TestNavigationKeepsExactBlobParents(t *testing.T) {
	b := testBackend(t)
	base := loc(t, "azure://account/c/prefix")
	for _, key := range []string{"prefix/", "prefix/.", "prefix/.."} {
		u := base.url.WithPathPart("c/" + key)
		b.remote = listedBlobs{blobStore: b.remote, nodes: []*store.Node{{URL: u, Kind: store.KindDir}}}
		entries, err := b.list(t.Context(), base)
		if err != nil || len(entries) != 1 || entries[0].loc.url.Key != key {
			t.Fatalf("listed %q: %v, %v", key, entries, err)
		}
		parent := parentLocation(entries[0].loc)
		if parent.display() != base.display() || parent.url.Key != base.url.Key {
			t.Fatalf("lost exact parent of %q: %+v", key, parent)
		}
	}
}
