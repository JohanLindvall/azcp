package azure

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// fakeBlobs is enough of the blob service for the naming operations: container
// and blob properties, flat and hierarchical listings, a block blob written in
// one request, and deletion. It counts what it is asked, by kind.
type fakeBlobs struct {
	mu         sync.Mutex
	containers map[string]map[string]fakeBlob // container -> blob name -> blob
	calls      map[string]int
}

type fakeBlob struct {
	data        []byte
	contentType string
	meta        map[string]string
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{containers: map[string]map[string]fakeBlob{}, calls: map[string]int{}}
}

// put stores a blob, creating its container.
func (f *fakeBlobs) put(container, name string, data []byte, meta map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containers[container] == nil {
		f.containers[container] = map[string]fakeBlob{}
	}
	f.containers[container][name] = fakeBlob{data: data, contentType: "application/octet-stream", meta: meta}
}

func (f *fakeBlobs) has(container, name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.containers[container][name]
	return ok
}

func (f *fakeBlobs) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[kind]
}

const httpDate = "Mon, 01 Jan 2024 00:00:00 GMT"

func stamp(w http.ResponseWriter) {
	w.Header().Set("Last-Modified", httpDate)
	w.Header().Set("ETag", `"0x1"`)
}

func refuse(w http.ResponseWriter, status int, code string) {
	w.Header().Set("x-ms-error-code", code)
	http.Error(w, "<Error><Code>"+code+"</Code></Error>", status)
}

func (f *fakeBlobs) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("x-ms-version", "2025-01-05")

	// Path-style: /account/container/blob.
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	container, blob := "", ""
	if len(parts) > 1 {
		container = parts[1]
	}
	if len(parts) > 2 {
		blob = parts[2]
	}
	q := r.URL.Query()
	blobs, exists := f.containers[container]

	switch {
	case container == "" && q.Get("comp") == "list":
		f.calls["list containers"]++
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Containers>`)
		for _, name := range slices.Sorted(maps.Keys(f.containers)) {
			fmt.Fprintf(&b, `<Container><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified>`+
				`<Etag>0x1</Etag></Properties></Container>`, name, httpDate)
		}
		b.WriteString(`</Containers><NextMarker/></EnumerationResults>`)
		writeXML(w, b.String())

	case q.Get("restype") == "container" && q.Get("comp") == "list":
		f.calls["list"]++
		if !exists {
			refuse(w, http.StatusNotFound, "ContainerNotFound")
			return
		}
		writeXML(w, listXML(container, blobs, q))

	case q.Get("restype") == "container":
		f.calls[r.Method+" container"]++
		if r.Method == http.MethodPut {
			if exists {
				refuse(w, http.StatusConflict, "ContainerAlreadyExists")
				return
			}
			f.containers[container] = map[string]fakeBlob{}
			stamp(w)
			w.WriteHeader(http.StatusCreated)
			return
		}
		if !exists {
			refuse(w, http.StatusNotFound, "ContainerNotFound")
			return
		}
		stamp(w)
		w.WriteHeader(http.StatusOK)

	case blob != "":
		f.calls[r.Method+" blob"]++
		if !exists {
			refuse(w, http.StatusNotFound, "ContainerNotFound")
			return
		}
		b, ok := blobs[blob]
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			meta := map[string]string{}
			for k, v := range r.Header {
				if name, isMeta := strings.CutPrefix(strings.ToLower(k), "x-ms-meta-"); isMeta {
					meta[name] = v[0]
				}
			}
			blobs[blob] = fakeBlob{data: data, contentType: r.Header.Get("x-ms-blob-content-type"), meta: meta}
			stamp(w)
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			if !ok {
				refuse(w, http.StatusNotFound, "BlobNotFound")
				return
			}
			delete(blobs, blob)
			w.WriteHeader(http.StatusAccepted)
		default: // properties, or the content
			if !ok {
				refuse(w, http.StatusNotFound, "BlobNotFound")
				return
			}
			stamp(w)
			w.Header().Set("Content-Length", strconv.Itoa(len(b.data)))
			w.Header().Set("Content-Type", b.contentType)
			w.Header().Set("x-ms-blob-type", "BlockBlob")
			for k, v := range b.meta {
				w.Header().Set("x-ms-meta-"+k, v)
			}
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(b.data)
			}
		}

	default:
		refuse(w, http.StatusBadRequest, "InvalidUri")
	}
}

// listXML answers a flat or hierarchical listing the way the service does:
// names under the prefix in order, with those beyond the delimiter folded into
// one BlobPrefix each.
func listXML(container string, blobs map[string]fakeBlob, q url.Values) string {
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	withMeta := strings.Contains(q.Get("include"), "metadata")
	limit := len(blobs) + 1
	if m := q.Get("maxresults"); m != "" {
		limit, _ = strconv.Atoi(m)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>`+
		`<EnumerationResults ContainerName="%s"><Blobs>`, container)
	seenPrefix := map[string]bool{}
	n := 0
	for _, name := range slices.Sorted(maps.Keys(blobs)) {
		if !strings.HasPrefix(name, prefix) || n >= limit {
			continue
		}
		rest := name[len(prefix):]
		if delim != "" {
			if i := strings.Index(rest, delim); i >= 0 {
				p := prefix + rest[:i+len(delim)]
				if !seenPrefix[p] {
					seenPrefix[p] = true
					fmt.Fprintf(&b, `<BlobPrefix><Name>%s</Name></BlobPrefix>`, p)
					n++
				}
				continue
			}
		}
		bl := blobs[name]
		fmt.Fprintf(&b, `<Blob><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified>`+
			`<Etag>0x1</Etag><Content-Length>%d</Content-Length><Content-Type>%s</Content-Type>`+
			`<BlobType>BlockBlob</BlobType></Properties>`, name, httpDate, len(bl.data), bl.contentType)
		if withMeta && len(bl.meta) > 0 {
			b.WriteString("<Metadata>")
			for _, k := range slices.Sorted(maps.Keys(bl.meta)) {
				fmt.Fprintf(&b, "<%s>%s</%s>", k, bl.meta[k], k)
			}
			b.WriteString("</Metadata>")
		}
		b.WriteString("</Blob>")
		n++
	}
	b.WriteString(`</Blobs><NextMarker/></EnumerationResults>`)
	return b.String()
}

// fakeStore serves f over HTTP and returns a store pointed at it, plus a way
// of naming things in it.
func fakeStore(t *testing.T, f *fakeBlobs, createContainers bool) (*Store, func(string) *uri.URL) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s := New(Config{
		Auth:            AuthAnonymous,
		Log:             slog.New(slog.DiscardHandler),
		CreateContainer: createContainers,
		IncludeMetadata: true,
	})
	at := func(p string) *uri.URL {
		u, err := uri.Parse(srv.URL+"/devstoreaccount1/"+p, uri.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	return s, at
}

func names(nodes []*store.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name()
		if n.IsDir() {
			out[i] += "/"
		}
	}
	return out
}

// A name is a blob, a prefix with something under it, or nothing; the
// container and the account root are directories. What is not there says
// which of those it was, since a missing container and a missing blob point
// at different mistakes.
func TestStatTellsBlobsPrefixesAndNothingApart(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "a/b.txt", []byte("hello"), map[string]string{"azcp_mode": "0644"})
	f.put("c", "a/deep/c.txt", nil, nil)
	s, at := fakeStore(t, f, false)
	ctx := context.Background()

	n, err := s.Stat(ctx, at("c/a/b.txt"), false)
	if err != nil {
		t.Fatal(err)
	}
	if !n.IsRegular() || n.Size != 5 || n.Name() != "b.txt" {
		t.Errorf("blob node = %+v", n)
	}
	if p := store.DecodePosixMeta(n.Metadata); !p.HasMode || p.Mode.Perm() != 0o644 {
		t.Errorf("metadata %v did not carry the mode", n.Metadata)
	}

	for _, p := range []string{"c/a", "c/a/", "c", ""} {
		n, err := s.Stat(ctx, at(p), false)
		if err != nil || !n.IsDir() {
			t.Errorf("Stat(%q) = %v, %v; want a directory", p, n, err)
		}
	}
	// A trailing slash says "prefix", so the blob is never asked about.
	heads := f.count("HEAD blob")
	if _, err := s.Stat(ctx, at("c/a/"), false); err != nil {
		t.Fatal(err)
	}
	if f.count("HEAD blob") != heads {
		t.Error("a name ending in / was looked up as a blob")
	}

	if _, err := s.Stat(ctx, at("c/nope"), false); !store.IsNotExist(err) {
		t.Errorf("a missing blob gave %v", err)
	}
	for _, p := range []string{"other/x", "other/x/", "other"} {
		_, err := s.Stat(ctx, at(p), false)
		if !store.IsNotExist(err) || !strings.Contains(err.Error(), "ContainerNotFound") {
			t.Errorf("Stat(%q) in a missing container gave %v", p, err)
		}
	}
}

// One level at a time, in one lexical sequence, the directory first where a
// blob and a prefix share a name.
func TestReadDirListsOneLevelDirectoriesFirst(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "b.txt", []byte("1"), nil)
	f.put("c", "a", []byte("a blob called a"), nil)
	f.put("c", "a/x.txt", []byte("2"), map[string]string{"k": "v"})
	f.put("c", "a/y/z.txt", []byte("3"), nil)
	f.put("c", "d/", nil, nil)
	f.put("other", "o.txt", nil, nil)
	s, at := fakeStore(t, f, false)
	ctx := context.Background()

	top, err := s.ReadDir(ctx, at("c"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(top), []string{"a/", "b.txt", "d/"}; !slices.Equal(got, want) {
		t.Errorf("ReadDir(c) = %v, want %v", got, want)
	}
	under, err := s.ReadDir(ctx, at("c/a"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(under), []string{"x.txt", "y/"}; !slices.Equal(got, want) {
		t.Errorf("ReadDir(c/a) = %v, want %v", got, want)
	}
	if under[0].Metadata["k"] != "v" {
		t.Errorf("the listing did not carry metadata: %v", under[0].Metadata)
	}
	if f.count("list") != 2 {
		t.Errorf("two directories cost %d listings", f.count("list"))
	}

	root, err := s.ReadDir(ctx, at(""))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(root), []string{"c/", "other/"}; !slices.Equal(got, want) {
		t.Errorf("ReadDir(account) = %v, want %v", got, want)
	}
	if _, err := s.ReadDir(ctx, at("missing")); !store.IsNotExist(err) {
		t.Errorf("a missing container listed as %v", err)
	}
}

// Containers are the one part of the namespace that has to exist. They are
// made only when asked, and checked once per run rather than once per file.
func TestMkdirAllCreatesTheContainerOnlyWhenAsked(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "x.txt", nil, nil)
	ctx := context.Background()

	s, at := fakeStore(t, f, false)
	if err := s.MkdirAll(ctx, at("c/dir"), 0); err != nil {
		t.Errorf("an existing container was refused: %v", err)
	}
	err := s.MkdirAll(ctx, at("new/dir"), 0)
	if err == nil || !strings.Contains(err.Error(), "--create-container") {
		t.Errorf("a missing container gave %v; want a hint at --create-container", err)
	}
	if f.has("new", "") || f.count("PUT container") != 0 {
		t.Error("a container was created without being asked for")
	}

	checks := func() int { return f.count("GET container") + f.count("HEAD container") }
	s, at = fakeStore(t, f, true)
	before := checks()
	for _, p := range []string{"new/dir", "new/other", "new"} {
		if err := s.MkdirAll(ctx, at(p), 0); err != nil {
			t.Errorf("MkdirAll(%q): %v", p, err)
		}
	}
	if f.count("PUT container") != 1 {
		t.Errorf("the container was created %d times", f.count("PUT container"))
	}
	if checks()-before != 1 {
		t.Errorf("three uses of one container cost %d checks; want 1", checks()-before)
	}
	before = checks()
	if err := s.MkdirAll(ctx, at(""), 0); err != nil || checks() != before {
		t.Errorf("the account root cost a request: %v", err)
	}
}

// An empty directory is a zero-byte marker blob, and removing the directory
// means removing the marker. A container is never removed.
func TestMkdirMarkerAndRemove(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "keep.txt", []byte("x"), nil)
	s, at := fakeStore(t, f, false)
	ctx := context.Background()

	if err := s.MkdirMarker(ctx, at("c/empty")); err != nil {
		t.Fatal(err)
	}
	if !f.has("c", "empty/") {
		t.Error("no marker blob was written")
	}
	if err := s.MkdirMarker(ctx, at("c")); err != nil || f.has("c", "") || f.has("c", "/") {
		t.Errorf("a marker for the container itself: %v", err)
	}

	if err := s.Remove(ctx, at("c/keep.txt")); err != nil || f.has("c", "keep.txt") {
		t.Errorf("Remove of a blob: %v", err)
	}
	if err := s.Remove(ctx, at("c/empty")); err != nil || f.has("c", "empty/") {
		t.Errorf("Remove of an empty directory left its marker: %v", err)
	}
	if err := s.Remove(ctx, at("c/missing")); !store.IsNotExist(err) {
		t.Errorf("Remove of nothing gave %v", err)
	}
	if err := s.Remove(ctx, at("c")); err == nil || !strings.Contains(err.Error(), "container") {
		t.Errorf("Remove of a container: %v", err)
	}
	if f.count("DELETE container") != 0 {
		t.Error("a container deletion was attempted")
	}
}
