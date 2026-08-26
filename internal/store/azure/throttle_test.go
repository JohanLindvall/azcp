package azure

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

// shortReader hands over at most chunk bytes per Read, the way a socket hands
// over one segment at a time. A local emulator does not behave like this — it
// fills whatever buffer it is given — which is why a throttle can look exact
// against Azurite and be wildly wrong against a real account.
type shortReader struct {
	left  int
	chunk int
}

func (r *shortReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), min(r.chunk, r.left))
	r.left -= n
	return n, nil
}

func (r *shortReader) Close() error { return nil }

// A stream must be paced by the bytes that arrive, not by the size of the
// buffer offered for them.
func TestThrottlePacesBytesNotBuffers(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s
	const total = 512 << 10
	const chunk = 1500 // one ethernet frame's worth per read
	const buffer = 32 << 10

	lim := newLimiter(rate)
	tr := &throttledReader{
		r:   &shortReader{left: total, chunk: chunk},
		lim: lim,
		ctx: context.Background(),
	}

	start := time.Now()
	n, err := io.CopyBuffer(io.Discard, tr, make([]byte, buffer))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Fatalf("read %d bytes, want %d", n, total)
	}

	// The bucket starts full, so the first burst is not paced at all; what
	// follows is.
	want := time.Duration((total - lim.burst) / rate * float64(time.Second))
	switch {
	case elapsed > 3*want:
		// The failure this guards against costs buffer/chunk — twenty times
		// over, not a few percent — so the bound can be loose enough to
		// survive a busy machine.
		t.Errorf("%d bytes at %d B/s took %v, want about %v: the stream is being "+
			"charged for buffers offered rather than bytes delivered",
			total, rate, elapsed.Round(time.Millisecond), want.Round(time.Millisecond))
	case elapsed < want/2:
		t.Errorf("%d bytes at %d B/s took %v, want about %v: the limit is not "+
			"being applied", total, rate, elapsed.Round(time.Millisecond),
			want.Round(time.Millisecond))
	}
	t.Logf("%d bytes in %v (want about %v), effective %.2f MiB/s of a %d MiB/s limit",
		total, elapsed.Round(time.Millisecond), want.Round(time.Millisecond),
		float64(total)/elapsed.Seconds()/(1<<20), rate>>20)
}

// stubTransport answers with a body of the given size.
type stubTransport struct{ size int }

func (s stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(make([]byte, s.size))),
		Request:    req,
	}, nil
}

// Listing a large container is megabytes of XML with every transfer in the run
// waiting behind it. Pacing that spends the whole limit on working out what to
// copy, so only the content is paced.
func TestOnlyContentIsThrottled(t *testing.T) {
	tr := &throttledTransport{base: stubTransport{size: 1024}, lim: newLimiter(64 << 10)}

	for _, tc := range []struct {
		name, url string
		paced     bool
	}{
		{"a container listing", "https://acct.blob.core.windows.net/c?restype=container&comp=list", false},
		{"an account listing", "https://acct.blob.core.windows.net/?comp=list", false},
		{"the uncommitted block list a resumed upload asks for",
			"https://acct.blob.core.windows.net/c/b?comp=blocklist", false},
		{"blob properties", "https://acct.blob.core.windows.net/c/b?comp=metadata", false},
		{"a blob read", "https://acct.blob.core.windows.net/c/b", true},
		{"a blob read carrying a SAS",
			"https://acct.blob.core.windows.net/c/b?sv=2024-11-04&sr=b&sig=YWJjZA%3D%3D", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, paced := resp.Body.(*throttledReader); paced != tc.paced {
				t.Errorf("paced = %v, want %v", paced, tc.paced)
			}
		})
	}
}
