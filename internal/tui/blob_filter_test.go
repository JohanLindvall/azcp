// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestBlobFilterTerms(t *testing.T) {
	n := &store.Node{
		URL: loc(t, "azure://account/c/a.bin").url, Kind: store.KindFile,
		ContentType: "image/png", ContentEncoding: "gzip", ContentLanguage: "en-GB",
		ContentDisposition: `attachment; filename="photo.png"`, CacheControl: "private, max-age=60",
		Tags:       map[string]string{"Project": "Alpha", "project": "beta", "empty": "", "a/b:c=d +": "value"},
		AccessTier: "Cool", ETag: `"0x123"`,
		Metadata: map[string]string{"Owner": "Alice", "empty": "", "path": "a/b/c", "batch_2": "nightly",
			"literal": "a*?[x]", "comparison": ">=10"},
	}
	for _, tc := range []struct {
		filter string
		want   bool
	}{
		{"content-type:image/*", true},
		{"tags.Project=Alpha", true},
		{"TAGS.Project=Alpha", true},
		{"tags.Project=alpha", false},
		{"tags.project=beta", true},
		{"tags.PROJECT:*", false},
		{"tags.Project:ALP", true},
		{"tags.Project:A*", true},
		{"tags.empty=", true},
		{"tags.empty:*", true},
		{"tags.missing=", false},
		{"-tags.Project=Alpha", false},
		{`"tags[a/b:c=d +]"=value`, true},
		{"CONTENT-TYPE:PNG", true},
		{"content-type=image/png", true},
		{"content-type=IMAGE/PNG", false},
		{"content-type:image/@(png|jpeg)", true},
		{"content-type:{image,text}/*", true},
		{"content-type:IMAGE/*", false},
		{"content-type:video/*", false},
		{"content-type=image/*", false},
		{"content-type=", false},
		{"content-encoding=gzip content-language:EN", true},
		{`content-disposition="attachment; filename=\"photo.png\""`, true},
		{`cache-control:"max-age=60" access-tier=Cool etag=\"0x123\"`, true},
		{"metadata.owner=Alice", true},
		{"METADATA.OWNER:alice", true},
		{"metadata.owner=alice", false},
		{"metadata.owner:Ali*", true},
		{"metadata.batch_2=nightly", true},
		{"metadata.path:a/*", true},
		{"metadata.literal=a*?[x]", true},
		{`metadata.literal:a\*\?\[x\]`, true},
		{"metadata.comparison:>=1", true},
		{"metadata.empty=", true},
		{"metadata.empty:*", true},
		{"metadata.missing=", false},
		{"metadata.missing:*", false},
		{"-metadata.missing:*", true},
		{"-metadata.empty:*", false},
		{"content-type:image/* metadata.owner=Alice", true},
		{"content-type:image/* metadata.owner=Bob", false},
		{"content-type:image/* content-type:video/*", false},
		{"content-type:image/* -metadata.owner=Alice", false},
		{"*.bin *.txt content-type:image/*", true},
		{"*.txt content-type:image/*", false},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			f, err := parseFilter(tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			if f.walks {
				t.Fatal("blob fields should not trigger a directory walk")
			}
			e := entry{name: "a.bin", node: n}
			if got := f.selects(e); got != tc.want {
				t.Errorf("match = %v, want %v", got, tc.want)
			}
			cached := entry{name: e.name, usage: &usageNode{kind: n.Kind, blob: cacheBlobValues(n, f.blobFields)}}
			if got := f.selects(cached); got != tc.want {
				t.Errorf("cached match = %v, want %v", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"tags.:x", "tags[]=x", "tags[broken", "tags.Project>x", "tags.Project:", "metadata.:x", "metadata.=x", "content-type:", "metadata.owner:",
		"content-type>image/png", "metadata.owner<=Alice", `content-type:"image/png`} {
		if _, err := parseFilter(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	f, err := parseFilter("content-type:image/* -content-type:text/* METADATA.OWNER:ali metadata.owner=Alice")
	if err != nil || !slices.Equal(f.blobFields, []string{"content-type", "metadata.owner"}) {
		t.Fatalf("fields were not deduplicated: %v, %v", f.blobFields, err)
	}
}

func TestBlobFiltersDoNotGuessLocalTypesOrInheritDirectoryProperties(t *testing.T) {
	for _, filter := range []string{"content-type:image/*", "metadata.owner=alice"} {
		f, err := parseFilter(filter)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			url  *uri.URL
			kind store.Kind
			want bool
		}{
			{"local", loc(t, t.TempDir()).url.Join("photo.png"), store.KindFile, false},
			{"prefix", loc(t, "azure://account/c/prefix").url, store.KindDir, false},
			{"container", loc(t, "azure://account/c").url, store.KindDir, false},
			{"symlink", loc(t, "azure://account/c/link").url, store.KindSymlink, true},
			{"missing URL", nil, store.KindFile, false},
		} {
			n := &store.Node{URL: tc.url, Kind: tc.kind, ContentType: "image/png", Metadata: map[string]string{"owner": "alice"}}
			if got := f.selects(entry{name: "photo.png", node: n}); got != tc.want {
				t.Errorf("%s, %s: match = %v", filter, tc.name, got)
			}
		}
	}
}

// A listing supplies blob properties without a HEAD per entry. An inventory
// can report changed properties, just as the service can between two requests.
type filterBlobs struct {
	*sizeBlobs
	stats, lists int
	fresh        func(*store.Node)
	removed      []*store.Node
	block        chan struct{}
}

func (s *filterBlobs) Stat(_ context.Context, u *uri.URL, _ bool) (*store.Node, error) {
	s.stats++
	if u.Key == "" {
		return &store.Node{URL: u, Kind: store.KindDir}, nil
	}
	for _, n := range s.nodes {
		if n.URL.Key == u.Key {
			fresh := *n
			if s.fresh != nil {
				s.fresh(&fresh)
			}
			return &fresh, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (s *filterBlobs) ReadDirEntries(context.Context, *uri.URL) ([]*store.Node, error) {
	s.lists++
	return s.nodes, nil
}

func (s *filterBlobs) WalkAll(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
	if s.block != nil {
		close(s.block)
		<-ctx.Done()
		return ctx.Err()
	}
	return s.sizeBlobs.WalkAll(ctx, u, onError, fn)
}

func (s *filterBlobs) RemoveBlob(_ context.Context, n *store.Node) error {
	s.removed = append(s.removed, n)
	return nil
}

func blobFilterFixture(t *testing.T) (location, *filterBlobs) {
	t.Helper()
	base := loc(t, "azure://account/c")
	s := &filterBlobs{sizeBlobs: &sizeBlobs{}}
	for i, v := range []struct{ name, contentType, owner string }{
		{"a.bin", "image/png", "alice"}, {"b.bin", "image/jpeg", "bob"}, {"c.png", "text/plain", "alice"},
	} {
		s.nodes = append(s.nodes, &store.Node{URL: base.url.Join(v.name), Kind: store.KindFile, Size: int64(5 - i),
			ContentType: v.contentType, Metadata: map[string]string{"owner": v.owner}, Tags: map[string]string{"Owner": v.owner}, ETag: "listed"})
	}
	return base, s
}

func TestCLISelectAndListUseListedBlobFields(t *testing.T) {
	base, remote := blobFilterFixture(t)
	b := testBackend(t)
	b.remote = remote
	f, err := parseFilter("content-type:image/* metadata.owner=alice")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := b.selectEach(t.Context(), f, []string{base.url.String()})
	if err != nil || len(selected) != 1 || selected[0].name != "a.bin" {
		t.Fatalf("select: %v, %v", selected, err)
	}
	rows, err := b.listOne(t.Context(), &f, base.url.String())
	if err != nil || len(rows.targets) != 1 || rows.targets[0].name != "a.bin" {
		t.Fatalf("list: %v, %v", rows, err)
	}
	if remote.stats != 2 || remote.lists != 2 || len(remote.walks) != 0 {
		t.Fatalf("selection fetched per-blob properties or walked: stats=%d lists=%d walks=%d", remote.stats, remote.lists, len(remote.walks))
	}
	for _, name := range []string{"a.bin", "c.png"} {
		rows, err := b.listOne(t.Context(), &f, base.url.Join(name).String())
		if err != nil || (len(rows.targets) == 1) != (name == "a.bin") {
			t.Fatalf("explicit blob %s: %v, %v", name, rows, err)
		}
	}
}

func TestPaneBlobFilterUsesLoadedEntries(t *testing.T) {
	a, _, _, _ := testApp(t)
	base, remote := blobFilterFixture(t)
	a.backend.remote = remote
	entries, err := a.backend.list(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	a.setEntries(0, base, entries)
	a.applyFilter("content-type:image/* metadata.owner=alice")
	if a.busy != nil || strings.Join(entryNames(a.panes[0].entries), " ") != ".. a.bin" {
		t.Fatalf("pane: %v, busy=%v", entryNames(a.panes[0].entries), a.busy != nil)
	}
	if remote.stats != 0 || remote.lists != 1 || len(remote.walks) != 0 {
		t.Fatal("applying a pane filter made extra storage requests")
	}
}

func TestUsageBlobFilterCachesOnlyRequestedFieldsAndReusesThem(t *testing.T) {
	a, _, _, _ := testApp(t)
	base, remote := blobFilterFixture(t)
	a.backend.remote = remote
	a.setEntries(0, base, nil)
	a.toggleUsage()
	finishJobs(t, a)
	p := &a.panes[0]
	for _, n := range p.usage.root.children {
		if n.blob != nil {
			t.Fatal("an unfiltered usage scan retained blob properties")
		}
	}
	for _, tc := range []struct {
		filter, names string
		walks         int
	}{
		{"content-type:image/*", ".. a.bin b.bin", 2},
		{"content-type:image/png", ".. a.bin", 2},
		{"metadata.owner=bob", ".. b.bin", 3},
		{"content-type:text/*", ".. c.png", 3},
		{"content-type:image/* metadata.owner=alice", ".. a.bin", 3},
	} {
		a.applyFilter(tc.filter)
		finishJobs(t, a)
		if got := strings.Join(entryNames(p.entries), " "); got != tc.names || len(remote.walks) != tc.walks {
			t.Fatalf("%s: entries=%s walks=%d, want %s and %d", tc.filter, got, len(remote.walks), tc.names, tc.walks)
		}
	}
	// Refreshing both panes takes another scan path than refreshing the
	// active usage pane. Both must retain the fields the filter needs.
	a.reload()
	finishJobs(t, a)
	if strings.Join(entryNames(p.entries), " ") != ".. a.bin" {
		t.Fatal("refreshing panes lost the blob properties required by the active filter")
	}
	a.applyFilter("metadata.owner=bob")
	if a.busy != nil || strings.Join(entryNames(p.entries), " ") != ".. b.bin" {
		t.Fatal("refreshing panes discarded previously cached fields")
	}
	updated, err := p.usage.without(t.Context(), []entry{p.usage.entry(findUsage(t, p.usage.root, "b.bin", store.KindFile))})
	if err != nil {
		t.Fatal(err)
	}
	a.setUsage(0, updated, "")
	a.applyFilter("metadata.owner=alice")
	if a.busy != nil || strings.Join(entryNames(p.entries), " ") != ".. a.bin c.png" {
		t.Fatal("updating the usage cache lost cached blob fields")
	}
}

func BenchmarkBlobFilterCache(b *testing.B) {
	n := &store.Node{URL: loc(b, "azure://account/c/file").url,
		ContentType: "image/png", Metadata: map[string]string{"owner": "alice"}}
	for _, fields := range [][]string{nil, {"metadata.missing"}, {"content-type", "metadata.owner"}} {
		name := strings.Join(fields, "+")
		if name == "" {
			name = "unfiltered"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				cacheBlobValues(n, fields)
			}
		})
	}
}

func TestCancelledBlobFilterScanKeepsThePreviousFilter(t *testing.T) {
	a, _, _, _ := testApp(t)
	base, remote := blobFilterFixture(t)
	a.backend.remote = remote
	a.setEntries(0, base, nil)
	a.toggleUsage()
	finishJobs(t, a)
	a.applyFilter("*.bin")
	p := &a.panes[0]
	before := p.usage
	remote.block = make(chan struct{})
	a.applyFilter("metadata.owner=alice")
	select {
	case <-remote.block:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not start")
	}
	press(a, tcell.KeyEscape, 0)
	finishJobs(t, a)
	if p.usage != before || p.filter.text != "*.bin" || strings.Join(entryNames(p.entries), " ") != ".. a.bin b.bin" {
		t.Fatal("cancelling published a filter without the properties needed to apply it")
	}
}

func TestLocalUsageBlobFilterDoesNotRescan(t *testing.T) {
	a, _, left, _ := testApp(t)
	write(t, filepath.Join(left, "photo.png"), "local file")
	a.toggleUsage()
	finishJobs(t, a)
	before := a.panes[0].usage
	a.applyFilter("content-type:image/*")
	if a.busy != nil || a.panes[0].usage != before || len(a.panes[0].entries) != 1 {
		t.Fatal("a blob filter rescanned a local tree or invented local blob properties")
	}
}

func TestRemovalRechecksBlobPropertiesAndUsesFreshETags(t *testing.T) {
	for _, filter := range []string{"content-type:image/*", "metadata.owner=alice", "tags.Owner=alice"} {
		t.Run(filter, func(t *testing.T) {
			base, remote := blobFilterFixture(t)
			remote.fresh = func(n *store.Node) {
				n.ETag = "fresh"
				if n.URL.Key == "a.bin" {
					n.ContentType = "text/plain"
					n.Metadata = map[string]string{"owner": "bob"}
					n.Tags = map[string]string{"Owner": "bob"}
				}
			}
			b := testBackend(t)
			b.remote = remote
			o := batchOptions(base.url.String(), filter)
			o.Confirmed, o.Remove = true, true
			output(t)
			failed, err := b.removeBatch(t.Context(), o)
			if err != nil || failed != 0 || len(remote.removed) != 1 || remote.removed[0].URL.Key == "a.bin" || remote.removed[0].ETag != "fresh" {
				t.Fatalf("removed=%v failures=%d error=%v", remote.removed, failed, err)
			}
			if remote.stats != 3 || remote.lists != 1 || len(remote.walks) != 0 {
				t.Fatalf("unnecessary requests: stats=%d lists=%d walks=%d", remote.stats, remote.lists, len(remote.walks))
			}
		})
	}
}

func TestPaneTagFiltersReloadAndUsageCachesTheirValues(t *testing.T) {
	a, _, _, _ := testApp(t)
	base, remote := blobFilterFixture(t)
	a.backend.remote = remote
	entries, err := a.backend.list(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	a.setEntries(0, base, entries)
	a.applyFilter("tags.Owner=alice")
	finishJobs(t, a)
	if names := strings.Join(entryNames(a.panes[0].entries), " "); names != ".. a.bin c.png" {
		t.Fatal(names)
	}
	a.reload()
	finishJobs(t, a)
	if names := strings.Join(entryNames(a.panes[0].entries), " "); names != ".. a.bin c.png" {
		t.Fatal(names)
	}
	a.toggleUsage()
	finishJobs(t, a)
	a.applyFilter("tags.Owner=bob")
	finishJobs(t, a)
	if names := strings.Join(entryNames(a.panes[0].entries), " "); names != ".. b.bin" {
		t.Fatal(names)
	}
}

func TestMoveInventoryDetectsTagChangesWithoutAnETagChange(t *testing.T) {
	base, _ := blobFilterFixture(t)
	a := &store.Node{URL: base.url.Join("blob"), Kind: store.KindFile, ETag: "same", Tags: map[string]string{"Owner": "alice"}}
	b := *a
	b.Tags = map[string]string{"Owner": "bob"}
	if sameNode(a, &b) {
		t.Fatal("tag-only change went unnoticed")
	}
}
