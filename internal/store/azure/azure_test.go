package azure

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func mustURL(t *testing.T, s string) *uri.URL {
	t.Helper()
	u, err := uri.Parse(s, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A flat listing names blobs, not the prefixes between them; the walk has to
// invent every directory a filesystem would have had on the way down.
func TestAncestors(t *testing.T) {
	cases := []struct {
		base, name string
		want       []string
	}{
		{"", "a/b/c.txt", []string{"a", "a/b"}},
		{"a/", "a/b/c.txt", []string{"a/b"}},
		{"a/", "a/b/", []string{"a/b"}}, // an empty-directory marker
		{"", "top.txt", nil},
		{"x/", "a/b.txt", nil}, // outside the prefix being walked
	}
	for _, c := range cases {
		if got := ancestors(c.base, c.name); !slices.Equal(got, c.want) {
			t.Errorf("ancestors(%q, %q) = %v, want %v", c.base, c.name, got, c.want)
		}
	}
}

// A blob and a prefix can share a name. The listing is sorted directory-first
// on ties, so deduplication keeps the directory.
func TestDedupeByName(t *testing.T) {
	mk := func(name string, dir bool) *store.Node {
		n := &store.Node{URL: mustURL(t, "azure://acct/c/"+name), Kind: store.KindFile}
		if dir {
			n.Kind = store.KindDir
		}
		return n
	}
	out := dedupeByName([]*store.Node{mk("a", true), mk("a", false), mk("b", false)})
	if len(out) != 2 || !out[0].IsDir() || out[0].Name() != "a" || out[1].Name() != "b" {
		names := make([]string, len(out))
		for i, n := range out {
			names[i] = n.Name()
		}
		t.Errorf("deduped to %v", names)
	}
}

// The scan may use a quarter of the run's request budget, never fewer than one
// listing and never more than the cap.
func TestListAhead(t *testing.T) {
	for peak, want := range map[int]int{0: 1, 1: 1, 4: 1, 8: 2, 64: 16, 1 << 20: maxListAhead} {
		if got := New(Config{PeakRequests: peak}).listAhead(); got != want {
			t.Errorf("PeakRequests=%d: listAhead = %d, want %d", peak, got, want)
		}
	}
}

func TestSanitizeURL(t *testing.T) {
	u, err := url.Parse("https://acct.blob.core.windows.net/c/k?sv=1&sig=SECRET")
	if err != nil {
		t.Fatal(err)
	}
	got := sanitizeURL(u)
	if strings.Contains(got, "SECRET") || !strings.HasPrefix(got, "https://acct.blob.core.windows.net/c/k?") {
		t.Errorf("sanitizeURL = %q", got)
	}
	if sanitizeURL(nil) != "" {
		t.Error("a nil URL did not sanitise to nothing")
	}
}

// Both stores wrap their native "nothing there" errors so callers need only
// store.IsNotExist; the service's own code is kept because "container not
// found" and "blob not found" point at different mistakes.
func TestNotFoundBecomesNotExist(t *testing.T) {
	u := mustURL(t, "azure://acct/c/k")
	notFound := &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "BlobNotFound"}
	if !isNotFound(notFound) {
		t.Error("a 404 was not recognised")
	}
	if isNotFound(&azcore.ResponseError{StatusCode: http.StatusForbidden}) || isNotFound(nil) {
		t.Error("something other than a 404 was taken for one")
	}
	err := notExist(u, notFound)
	if !store.IsNotExist(err) || !strings.Contains(err.Error(), "BlobNotFound") {
		t.Errorf("notExist = %v", err)
	}
	if !store.IsNotExist(notExist(u, nil)) {
		t.Error("notExist without a cause is not IsNotExist")
	}
}

// A zero-byte blob whose name ends in "/" is an empty directory; anything else
// is a file, however little the listing said about it.
func TestItemNodeReadsWhatTheListingCarries(t *testing.T) {
	base := mustURL(t, "azure://acct/c")
	zero := int64(0)

	marker := itemNode(base, &container.BlobItem{
		Name: to.Ptr("dir/"), Properties: &container.BlobProperties{ContentLength: &zero},
	})
	if !marker.IsDir() || marker.Name() != "dir" {
		t.Errorf("marker blob became %v %q", marker.Kind, marker.Name())
	}

	empty := itemNode(base, &container.BlobItem{
		Name: to.Ptr("dir/file"),
		Properties: &container.BlobProperties{
			ContentLength: &zero, ContentType: to.Ptr("text/plain"),
			ContentEncoding: to.Ptr("gzip"), ETag: to.Ptr(azcore.ETag(`"abc"`)),
		},
		Metadata: map[string]*string{"azcp_mode": to.Ptr("0644"), "gone": nil},
	})
	if !empty.IsRegular() || empty.Name() != "file" || empty.ContentType != "text/plain" ||
		empty.ContentEncoding != "gzip" || empty.ETag != `"abc"` {
		t.Errorf("file node = %+v", empty)
	}
	if len(empty.Metadata) != 1 || empty.Metadata["azcp_mode"] != "0644" {
		t.Errorf("metadata = %v; a nil value should be dropped", empty.Metadata)
	}

	bare := itemNode(base, &container.BlobItem{Name: to.Ptr("dir/other")})
	if !bare.IsRegular() || bare.Size != 0 || bare.Metadata != nil {
		t.Errorf("a listing item without properties became %+v", bare)
	}
}
