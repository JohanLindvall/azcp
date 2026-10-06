// SPDX-License-Identifier: MIT

package azure

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/parallel"
	"github.com/JohanLindvall/azcp/internal/store"
)

// Writes that arrive out of order come out in order, and nothing is still
// held once everything has been written.
func TestOrderedWriterPutsWritesBackInOrder(t *testing.T) {
	var out bytes.Buffer
	o := newOrderedWriter(&out, 1<<20)
	for _, w := range []struct {
		off  int64
		text string
	}{{6, "ghi"}, {3, "def"}, {9, "jk"}, {0, "abc"}} {
		if n, err := o.WriteAt([]byte(w.text), w.off); err != nil || n != len(w.text) {
			t.Fatalf("write at %d: %d, %v", w.off, n, err)
		}
	}
	if out.String() != "abcdefghijk" {
		t.Errorf("wrote %q", out.String())
	}
	if o.written() != 11 || len(o.held) != 0 {
		t.Errorf("written %d, still holding %d", o.written(), len(o.held))
	}
}

// Ranges of a parallel download, each written in small pieces by its own
// worker, reach the destination exactly as they were in the blob.
func TestOrderedWriterUnderParallelRanges(t *testing.T) {
	data := make([]byte, 1<<20+77)
	if _, err := rand.NewChaCha8([32]byte{2}).Read(data); err != nil {
		t.Fatal(err)
	}
	const rangeSize = 64 << 10
	var out bytes.Buffer
	o := newOrderedWriter(&out, 4*rangeSize)
	count := (len(data) + rangeSize - 1) / rangeSize
	err := parallel.Do(context.Background(), count, 4, func(ctx context.Context, i int) error {
		off := i * rangeSize
		if err := o.admit(ctx, int64(off)); err != nil {
			return err
		}
		end := min(off+rangeSize, len(data))
		for off < end {
			n := min(1+rand.IntN(5000), end-off)
			if _, err := o.WriteAt(data[off:off+n], int64(off)); err != nil {
				return err
			}
			off += n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Error("the destination does not hold the blob as it was")
	}
}

// A range is admitted only within the window of what has already been
// written, which is what keeps the memory held to about a range per worker.
func TestOrderedWriterAdmitsWithinTheWindow(t *testing.T) {
	var out bytes.Buffer
	o := newOrderedWriter(&out, 4)
	ctx := context.Background()
	if err := o.admit(ctx, 3); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan error, 1)
	go func() { admitted <- o.admit(ctx, 4) }()
	select {
	case <-admitted:
		t.Fatal("admitted a range beyond the window")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := o.WriteAt([]byte("a"), 0); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the front moved")
	}

	cancelled, cancel := context.WithCancel(ctx)
	go func() { admitted <- o.admit(cancelled, 100) }()
	cancel()
	if err := <-admitted; !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait ended with %v", err)
	}
}

// A destination that refuses a write — a reader that went away — fails every
// write and every admission after it, including one already waiting.
func TestOrderedWriterRefusalIsFinal(t *testing.T) {
	refused := errors.New("broken pipe")
	o := newOrderedWriter(refusingWriter{refused}, 1)
	waiting := make(chan error, 1)
	go func() { waiting <- o.admit(context.Background(), 10) }()
	if _, err := o.WriteAt([]byte("a"), 0); !errors.Is(err, refused) {
		t.Fatalf("first write: %v", err)
	}
	if _, err := o.WriteAt([]byte("b"), 1); !errors.Is(err, refused) {
		t.Errorf("a later write: %v", err)
	}
	if err := <-waiting; !errors.Is(err, refused) {
		t.Errorf("a waiting admission: %v", err)
	}
	if !errors.Is(o.failure(), refused) {
		t.Errorf("failure reports %v", o.failure())
	}
}

type refusingWriter struct{ err error }

func (w refusingWriter) Write([]byte) (int, error) { return 0, w.err }

type partialWriter struct{ err error }

func (w partialWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }

func TestOrderedWriterCountsPartialWritesAndRejectsShortSuccess(t *testing.T) {
	for _, cause := range []error{nil, errors.New("pipe closed")} {
		o := newOrderedWriter(partialWriter{cause}, 64)
		n, err := o.WriteAt([]byte("abcd"), 0)
		want := cause
		if want == nil {
			want = io.ErrShortWrite
		}
		if n != 2 || o.written() != 2 || !errors.Is(err, want) {
			t.Fatalf("write = %d, %v; accounted %d bytes", n, err, o.written())
		}
		if _, err := o.WriteAt([]byte("ef"), 2); !errors.Is(err, want) {
			t.Fatal("short write was not final")
		}
	}
}

// rangeStart is where a ranged GET begins.
func rangeStart(r *http.Request) int64 {
	var start, end int64
	header := r.Header.Get("x-ms-range")
	if header == "" {
		header = r.Header.Get("Range")
	}
	if _, err := fmt.Sscanf(header, "bytes=%d-%d", &start, &end); err != nil {
		return 0
	}
	return start
}

// A blob downloaded into something that can only be written front to back
// arrives whole and in order, with its ranges still fetched side by side and
// its checksum taken on the way past. The first range answers last, so the
// others have to be held until it does.
func TestDownloadToWritesInOrder(t *testing.T) {
	data := make([]byte, 10*minBlockSize+123)
	if _, err := rand.NewChaCha8([32]byte{3}).Read(data); err != nil {
		t.Fatal(err)
	}
	var inFlight, most atomic.Int32
	var mu sync.Mutex
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		mu.Lock()
		if n > most.Load() {
			most.Store(n)
		}
		mu.Unlock()
		if rangeStart(r) == 0 {
			time.Sleep(100 * time.Millisecond)
		}
		rangeReply(w, r, data)
	})
	sum := md5.Sum(data)
	src := &store.Node{URL: u, Size: int64(len(data)), MD5: sum[:]}
	var out bytes.Buffer
	o := TransferOptions{BlockSize: minBlockSize, Concurrency: 4, CheckMD5: MD5Require}
	if err := s.DownloadTo(context.Background(), src, &out, o); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Error("what was written is not the blob")
	}
	if most.Load() < 2 {
		t.Errorf("at most %d range at a time", most.Load())
	}
}

// The checksum can be weighed only after the bytes have gone, but it is still
// weighed: a mismatch fails the copy, or under --check-md5=warn is noted.
func TestDownloadToChecksBytesOnTheWayPast(t *testing.T) {
	data := bytes.Repeat([]byte("pipe"), 3*minBlockSize/4)
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) { rangeReply(w, r, data) })
	wrong := md5.Sum([]byte("something else"))
	src := &store.Node{URL: u, Size: int64(len(data)), MD5: wrong[:]}
	o := TransferOptions{BlockSize: minBlockSize, Concurrency: 2, CheckMD5: MD5Fail}
	err := s.DownloadTo(context.Background(), src, &bytes.Buffer{}, o)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("a mismatch was accepted: %v", err)
	}
	o.CheckMD5 = MD5Warn
	if err := s.DownloadTo(context.Background(), src, &bytes.Buffer{}, o); err != nil {
		t.Errorf("--check-md5=warn failed the copy: %v", err)
	}
}
