package azure

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Dividing a listing must cover exactly what one listing would have: the
// ranges have to reach every key and no key twice. These are the shapes that
// get that wrong.
func TestSplitUnitsCoversEverythingOnce(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		units []string
		ways  int
		want  []string
	}{{
		name:  "a long shared prefix is cut where the names diverge",
		units: []string{"s/0a/", "s/0b/", "s/1a/", "s/1b/", "s/2a/", "s/2b/"},
		ways:  3,
		want:  []string{"s/0", "s/1", "s/2"},
	}, {
		name:  "the largest group is the one divided again",
		units: []string{"a/", "b0/", "b1/", "b2/", "b3/"},
		ways:  4,
		want:  []string{"a/", "b0/", "b1/", "b2/", "b3/"},
	}, {
		name:  "a single unit becomes its own name, not the shared prefix",
		units: []string{"only/one/"},
		ways:  4,
		want:  []string{"only/one/"},
	}, {
		name:  "a blob that is also the start of a prefix keeps the group whole",
		base:  "x/",
		units: []string{"x/ab", "x/ab.txt", "x/abc/"},
		ways:  4,
		want:  []string{"x/"},
	}, {
		name:  "one division may overshoot: its parts are all or none",
		units: []string{"a/", "b/", "c/", "d/"},
		ways:  2,
		want:  []string{"a/", "b/", "c/", "d/"},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitUnits(c.base, c.units, c.ways)
			var prefixes []string
			for _, r := range got {
				prefixes = append(prefixes, r.prefix)
			}
			if !slices.Equal(prefixes, c.want) {
				t.Errorf("ranges = %v, want %v", prefixes, c.want)
			}
			assertCovers(t, prefixes, c.units)
		})
	}
}

// assertCovers holds the property the whole scheme rests on: every unit falls
// inside exactly one range.
func assertCovers(t *testing.T, ranges, units []string) {
	t.Helper()
	for _, u := range units {
		n := 0
		for _, r := range ranges {
			if strings.HasPrefix(u, r) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%q is covered by %d ranges, want exactly 1 (%v)", u, n, ranges)
		}
	}
	for i := 1; i < len(ranges); i++ {
		if ranges[i-1] >= ranges[i] {
			t.Errorf("ranges are out of order: %q then %q", ranges[i-1], ranges[i])
		}
	}
}

// The first page has already been emitted, so the ranges below it are not
// listed at all and the one it ends inside is told where to pick up.
func TestAfterOnlySkipsWhatIsAlreadyEmitted(t *testing.T) {
	ranges := []keyRange{{prefix: "a"}, {prefix: "b"}, {prefix: "c"}, {prefix: "d"}}
	got := afterOnly(slices.Clone(ranges), "b/mid.txt")
	want := []keyRange{{prefix: "b", after: "b/mid.txt"}, {prefix: "c"}, {prefix: "d"}}
	if !slices.Equal(got, want) {
		t.Errorf("afterOnly = %v, want %v", got, want)
	}
}

// splitAccount serves one container of blobs with real paging and a working
// delimiter, so a divided listing can be checked against what an undivided one
// would have produced.
type splitAccount struct {
	names    []string // sorted
	pageSize int
	delay    time.Duration

	mu        sync.Mutex
	flat      int
	hierarchy int
	inFlight  int
	peak      int
}

func (a *splitAccount) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("restype") != "container" {
		writeXML(w, containersXML(1))
		return
	}

	a.mu.Lock()
	a.inFlight++
	a.peak = max(a.peak, a.inFlight)
	if q.Get("delimiter") != "" {
		a.hierarchy++
	} else {
		a.flat++
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.inFlight--
		a.mu.Unlock()
	}()
	time.Sleep(a.delay)

	prefix, marker := q.Get("prefix"), q.Get("marker")
	if q.Get("delimiter") == "" {
		writeXML(w, a.flatXML(prefix, marker))
		return
	}
	writeXML(w, a.hierarchyXML(prefix))
}

// matching is the window of names a request asks about.
func (a *splitAccount) matching(prefix, marker string) []string {
	var out []string
	for _, n := range a.names {
		if strings.HasPrefix(n, prefix) && n > marker {
			out = append(out, n)
		}
	}
	return out
}

func (a *splitAccount) flatXML(prefix, marker string) string {
	hits := a.matching(prefix, marker)
	next := ""
	if len(hits) > a.pageSize {
		hits = hits[:a.pageSize]
		next = hits[len(hits)-1]
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<EnumerationResults ContainerName="c000"><Blobs>`)
	for _, n := range hits {
		b.WriteString(blobXML(n))
	}
	fmt.Fprintf(&b, `</Blobs><NextMarker>%s</NextMarker></EnumerationResults>`, next)
	return b.String()
}

func (a *splitAccount) hierarchyXML(prefix string) string {
	seen := map[string]bool{}
	var dirs, blobs []string
	for _, n := range a.matching(prefix, "") {
		rest := n[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			if d := n[:len(prefix)+i+1]; !seen[d] {
				seen[d] = true
				dirs = append(dirs, d)
			}
			continue
		}
		blobs = append(blobs, n)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<EnumerationResults ContainerName="c000"><Blobs>`)
	for _, d := range dirs {
		fmt.Fprintf(&b, `<BlobPrefix><Name>%s</Name></BlobPrefix>`, d)
	}
	for _, n := range blobs {
		b.WriteString(blobXML(n))
	}
	b.WriteString(`</Blobs><NextMarker/></EnumerationResults>`)
	return b.String()
}

func blobXML(name string) string {
	// An empty directory is spelled as a zero-byte blob whose name ends in a
	// slash, so the fake has to spell it that way too.
	size := 7
	if strings.HasSuffix(name, "/") {
		size = 0
	}
	return fmt.Sprintf(`<Blob><Name>%s</Name><Properties>`+
		`<Last-Modified>Mon, 01 Jan 2024 00:00:00 GMT</Last-Modified><Etag>0x1</Etag>`+
		`<Content-Length>%d</Content-Length><BlobType>BlockBlob</BlobType>`+
		`</Properties></Blob>`, name, size)
}

func (a *splitAccount) walk(t *testing.T, peakRequests int) []string {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)

	s := New(Config{
		Auth:         AuthAnonymous,
		Log:          slog.New(slog.DiscardHandler),
		PeakRequests: peakRequests,
	})
	u, err := uri.Parse(srv.URL+"/devstoreaccount1/c000", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	err = s.WalkAll(context.Background(), u, func(*uri.URL, error) error { return nil },
		func(n *store.Node) error {
			if !n.IsDir() {
				got = append(got, strings.TrimPrefix(n.URL.PathPart(), "c000/"))
			}
			return nil
		})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return got
}

// exportShape is the layout that made a rerun slow: everything under one
// prefix, fanning out only a level below it.
func exportShape(subs, each int) []string {
	names := []string{"_commitments.json", "_prices.json"}
	for i := range subs {
		for j := range each {
			names = append(names,
				fmt.Sprintf("subscriptions/%04x-sub/2026/part-%04d.csv", i, j))
		}
	}
	sort.Strings(names)
	return names
}

// A container of more than a page is divided, and it has to reach exactly what
// an undivided listing would have: every blob, once. The sequence is the one
// thing that changes, and the ranges are what make it change.
func TestDividedListingReachesEveryBlobOnce(t *testing.T) {
	// Long enough that plenty is still to come once the division is due.
	names := exportShape(40, 100) // 4002 blobs, forty-one pages
	acct := &splitAccount{names: names, pageSize: 100}

	got := acct.walk(t, 64)
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	if !slices.Equal(sorted, names) {
		t.Fatalf("walked %d names, want %d; first difference at %d",
			len(got), len(names), firstDiff(sorted, names))
	}
	if acct.hierarchy == 0 {
		t.Error("the listing was never divided: no hierarchical listing was made")
	}
	if acct.peak < 2 {
		t.Errorf("peak concurrent listings = %d: the ranges were listed one at a time",
			acct.peak)
	}
}

// What a divided listing must still guarantee: a directory is emitted before
// anything inside it, which is what the empty-directory markers and the
// destination directories are built on.
func TestDividedListingEmitsDirectoriesFirst(t *testing.T) {
	names := exportShape(40, 100)
	acct := &splitAccount{names: names, pageSize: 100}
	srv := httptest.NewServer(acct)
	defer srv.Close()

	s := New(Config{Auth: AuthAnonymous, Log: slog.New(slog.DiscardHandler), PeakRequests: 64})
	u, err := uri.Parse(srv.URL+"/devstoreaccount1/c000", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The container itself is emitted by the account-level walk, not this one.
	dirs := map[string]bool{"c000": true}
	err = s.WalkAll(context.Background(), u, func(*uri.URL, error) error { return nil },
		func(n *store.Node) error {
			path := n.URL.PathPart()
			if n.IsDir() {
				dirs[path] = true
				return nil
			}
			for i := len(path) - 1; i >= 0; i-- {
				if path[i] == '/' && !dirs[path[:i]] {
					t.Errorf("%q arrived before its directory %q", path, path[:i])
					return nil
				}
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
}

// The division has to pay for itself. A container that fits in one page is
// never asked about a second time, which is what keeps an account of many
// small containers costing what it always did.
func TestOnePageContainerIsNeverDivided(t *testing.T) {
	names := exportShape(4, 5) // 22 blobs
	acct := &splitAccount{names: names, pageSize: 5000}

	if got := acct.walk(t, 64); !slices.Equal(got, names) {
		t.Fatalf("walked %v, want %v", got, names)
	}
	if acct.flat != 1 || acct.hierarchy != 0 {
		t.Errorf("made %d flat and %d hierarchical listings, want 1 and 0",
			acct.flat, acct.hierarchy)
	}
}

// Nor is a listing of a few pages, where the round trips saved would not cover
// the ones spent finding the ranges and finishing each of them off.
func TestAFewPagesIsNotDivided(t *testing.T) {
	names := exportShape(10, 10) // 102 blobs
	acct := &splitAccount{names: names, pageSize: 30}

	got := acct.walk(t, 64)
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	if !slices.Equal(sorted, names) {
		t.Fatalf("walked %d names, want %d", len(got), len(names))
	}
	if acct.hierarchy != 0 {
		t.Errorf("a listing of four pages was divided (%d hierarchical listings); "+
			"it costs more than it saves", acct.hierarchy)
	}
}

// Dividing is meant to save round trips, not spend them: the ranges must not
// cost wildly more requests than paging straight through would have.
func TestDividingStaysNearTheRequestFloor(t *testing.T) {
	names := exportShape(40, 100)
	const pageSize = 100
	acct := &splitAccount{names: names, pageSize: pageSize}
	acct.walk(t, 64)

	floor := (len(names) + pageSize - 1) / pageSize // pages, listed in a line
	total := acct.flat + acct.hierarchy
	if total > 2*floor {
		t.Errorf("dividing cost %d requests (%d flat, %d hierarchical); "+
			"paging straight through would have been %d",
			total, acct.flat, acct.hierarchy, floor)
	}
}

// A run with one job at a time asks for no parallelism anywhere, and dividing
// a listing is parallelism.
func TestDividingIsOffWhenNothingRunsInParallel(t *testing.T) {
	names := exportShape(40, 100)
	acct := &splitAccount{names: names, pageSize: 100}
	if got := acct.walk(t, 1); !slices.Equal(got, names) {
		t.Fatalf("walked %d names, want %d", len(got), len(names))
	}
	if acct.hierarchy != 0 {
		t.Errorf("made %d hierarchical listings with a look-ahead of one, want 0",
			acct.hierarchy)
	}
}

func firstDiff(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// An empty directory is a zero-byte blob whose name ends in "/", and it is the
// only thing that says the directory is there at all. The walk has to emit it
// as a directory of its own: its ancestors stop one level short.
func TestEmptyDirectoryMarkerBecomesADirectory(t *testing.T) {
	acct := &splitAccount{
		names:    []string{"kept/", "top.txt", "tree/deep/file.txt"},
		pageSize: 5000,
	}
	srv := httptest.NewServer(acct)
	defer srv.Close()

	s := New(Config{Auth: AuthAnonymous, Log: slog.New(slog.DiscardHandler), PeakRequests: 64})
	u, err := uri.Parse(srv.URL+"/devstoreaccount1/c000", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var dirs, files []string
	err = s.WalkAll(context.Background(), u, func(*uri.URL, error) error { return nil },
		func(n *store.Node) error {
			path := strings.TrimPrefix(n.URL.PathPart(), "c000/")
			if n.IsDir() {
				dirs = append(dirs, path)
			} else {
				files = append(files, path)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(dirs)
	slices.Sort(files)
	if want := []string{"kept", "tree", "tree/deep"}; !slices.Equal(dirs, want) {
		t.Errorf("directories = %v, want %v", dirs, want)
	}
	if want := []string{"top.txt", "tree/deep/file.txt"}; !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
}
