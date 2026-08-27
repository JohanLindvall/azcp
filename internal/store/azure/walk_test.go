package azure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// fakeAccount serves the two listings a scan makes, slowly enough that doing
// them one after another would show.
type fakeAccount struct {
	containers int
	blobs      int
	delay      time.Duration
	failOn     string // a container whose listing is refused

	mu       sync.Mutex
	inFlight int
	peak     int
}

func (f *fakeAccount) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.inFlight++
	f.peak = max(f.peak, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	time.Sleep(f.delay)

	if r.URL.Query().Get("restype") == "container" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		name := parts[len(parts)-1]
		if name == f.failOn {
			w.Header().Set("x-ms-error-code", "ContainerBeingDeleted")
			http.Error(w, "<Error><Code>ContainerBeingDeleted</Code></Error>", http.StatusConflict)
			return
		}
		writeXML(w, blobsXML(name, f.blobs))
		return
	}
	writeXML(w, containersXML(f.containers))
}

func (f *fakeAccount) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-ms-version", "2025-01-05")
	_, _ = w.Write([]byte(body))
}

func containersXML(n int) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Containers>`)
	for i := range n {
		fmt.Fprintf(&b, `<Container><Name>%s</Name><Properties>`+
			`<Last-Modified>Mon, 01 Jan 2024 00:00:00 GMT</Last-Modified><Etag>0x1</Etag>`+
			`</Properties></Container>`, containerName(i))
	}
	b.WriteString(`</Containers><NextMarker/></EnumerationResults>`)
	return b.String()
}

func blobsXML(container string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?>`+
		`<EnumerationResults ContainerName="%s"><Blobs>`, container)
	for i := range n {
		fmt.Fprintf(&b, `<Blob><Name>v_%02d.mp4</Name><Properties>`+
			`<Last-Modified>Mon, 01 Jan 2024 00:00:00 GMT</Last-Modified><Etag>0x1</Etag>`+
			`<Content-Length>1024</Content-Length><BlobType>BlockBlob</BlobType>`+
			`</Properties></Blob>`, i)
	}
	b.WriteString(`</Blobs><NextMarker/></EnumerationResults>`)
	return b.String()
}

func containerName(i int) string { return fmt.Sprintf("c%03d", i) }

func walkFake(t *testing.T, acct *fakeAccount, peakRequests int) ([]string, []error, error) {
	t.Helper()
	srv := httptest.NewServer(acct)
	t.Cleanup(srv.Close)

	s := New(Config{
		Auth:         AuthAnonymous,
		Log:          slog.New(slog.DiscardHandler),
		PeakRequests: peakRequests,
	})
	u, err := uri.Parse(srv.URL+"/devstoreaccount1", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	var failures []error
	err = s.WalkAll(context.Background(), u,
		func(_ *uri.URL, e error) error { failures = append(failures, e); return nil },
		func(n *store.Node) error { got = append(got, n.URL.PathPart()); return nil })
	return got, failures, err
}

// Containers are listed several at a time — the round trips are what a scan
// spends its life on — but what the caller sees is unchanged: one container
// after another, in the order they were listed, each followed by its contents.
func TestWalkListsAheadWithoutDisturbingTheOrder(t *testing.T) {
	const containers, blobs = 24, 3
	acct := &fakeAccount{containers: containers, blobs: blobs, delay: 20 * time.Millisecond}

	start := time.Now()
	got, failures, err := walkFake(t, acct, 32) // a look-ahead of 8
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}

	var want []string
	for i := range containers {
		want = append(want, containerName(i))
		for j := range blobs {
			want = append(want, fmt.Sprintf("%s/v_%02d.mp4", containerName(i), j))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("walked %d nodes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node %d is %q, want %q: the order a scan hands out has to be "+
				"the order it listed in", i, got[i], want[i])
		}
	}

	// A look-ahead of 8 means eight queued in front of the one being consumed.
	if peak := acct.peakConcurrency(); peak < 2 {
		t.Errorf("peak concurrent listings = %d: they are being done one at a time", peak)
	} else if peak > 9 {
		t.Errorf("peak concurrent listings = %d, want no more than the look-ahead "+
			"of 8 plus the one in hand", peak)
	}
	// Sequential would be 24 round trips of 20ms; anything near that means the
	// look-ahead is not working.
	if sequential := containers * 20 * time.Millisecond; elapsed > sequential/2 {
		t.Errorf("walk took %v, sequential would be about %v",
			elapsed.Round(time.Millisecond), sequential)
	}
}

// A container that cannot be listed is reported and the walk carries on, which
// is what it did when the listings were sequential.
func TestWalkReportsOneContainerAndCarriesOn(t *testing.T) {
	const containers, blobs = 8, 2
	acct := &fakeAccount{containers: containers, blobs: blobs, failOn: containerName(3)}

	got, failures, err := walkFake(t, acct, 32)
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(failures))
	}
	if failures[0] == nil || !strings.Contains(failures[0].Error(), "ContainerBeingDeleted") {
		t.Errorf("failure = %v, want the service's answer", failures[0])
	}
	// Every other container is still walked, and the failed one still appears
	// as a directory, because that much was known before it was listed.
	want := containers + (containers-1)*blobs
	if len(got) != want {
		t.Errorf("walked %d nodes, want %d", len(got), want)
	}
	for i, name := range got {
		if i > 0 && got[i-1] > name && !strings.Contains(name, "/") {
			t.Errorf("containers came back out of order at %d: %q after %q", i, name, got[i-1])
		}
	}
}

// Cancelling a scan has to stop it, not leave listings running behind it.
func TestWalkStopsWhenTheCallerDoes(t *testing.T) {
	acct := &fakeAccount{containers: 200, blobs: 2, delay: 5 * time.Millisecond}
	srv := httptest.NewServer(acct)
	defer srv.Close()

	s := New(Config{Auth: AuthAnonymous, Log: slog.New(slog.DiscardHandler), PeakRequests: 32})
	u, err := uri.Parse(srv.URL+"/devstoreaccount1", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}

	stop := errors.New("enough")
	seen := 0
	err = s.WalkAll(context.Background(), u, func(*uri.URL, error) error { return nil },
		func(*store.Node) error {
			seen++
			if seen == 5 {
				return stop
			}
			return nil
		})
	if !errors.Is(err, stop) {
		t.Fatalf("walk returned %v, want the caller's own error", err)
	}
}
