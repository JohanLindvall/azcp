package azure

import (
	"bytes"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"sync"

	"github.com/JohanLindvall/azcp/internal/store"
)

// A pipe, or a device such as a terminal, takes a download front to back or
// not at all. It cannot be sized beforehand, written out of order or read back
// to be checked, which is everything Download does to a file. One stream would
// be a fraction of what the link carries, so the ranges are still fetched in
// parallel; they are put back in order before they are written.

// DownloadTo writes a blob to w in order. The checksum is taken on the way
// past, so a mismatch can be reported only once the bytes have gone.
func (s *Store) DownloadTo(ctx context.Context, src *store.Node, w io.Writer, o TransferOptions) error {
	sum := md5.New()
	if wantsDigest(src.MD5, o.CheckMD5) {
		w = io.MultiWriter(sum, w)
	}
	var sent int64
	var failed error
	err := s.withSignIn(ctx, func() error {
		if sent > 0 {
			// A credential that is refused part-way cannot be answered by
			// starting again: what reached w would arrive twice.
			return failed
		}
		if src.Size == 0 {
			return nil
		}
		sum.Reset()
		ow := newOrderedWriter(w, int64(o.concurrency())*o.blockSize(src.Size))
		err := s.downloadRanges(ctx, src, ow, o, nil)
		sent = ow.written()
		if err == nil && sent != src.Size {
			err = fmt.Errorf("wrote %d of the %d bytes in %s", sent, src.Size, src.URL.Display())
		}
		if werr := ow.failure(); werr != nil {
			// The destination refused, which says more than the range that
			// happened to be writing when it did.
			err = werr
		}
		// Kept for a second attempt, which withSignIn makes whenever the
		// credential changed meanwhile, whatever this one failed of.
		failed = err
		return err
	})
	if err != nil {
		return err
	}
	return s.checkDigest(sum.Sum(nil), src.MD5, o.CheckMD5, src.URL.Display())
}

// orderedWriter puts the ranges of a parallel download back in order for a
// destination that can only be written front to back. Whatever arrives ahead
// of its turn is held in memory, and admit keeps a range from being fetched
// until it is within window bytes of the front, which is what bounds that
// memory: about one range per worker.
type orderedWriter struct {
	w      io.Writer
	window int64

	mu      sync.Mutex
	next    int64            // offset of the first byte not yet written
	held    map[int64][]byte // writes that arrived early, by offset
	writing bool             // a writer is draining; everyone else holds
	moved   chan struct{}    // closed and replaced whenever next advances
	err     error            // the destination's refusal, final once set
}

func newOrderedWriter(w io.Writer, window int64) *orderedWriter {
	return &orderedWriter{w: w, window: window, held: map[int64][]byte{}, moved: make(chan struct{})}
}

// admit waits until a range starting at off is close enough to the front to
// be held until its turn. Ranges are handed out in order, so the one at the
// front is always already being fetched and the wait always ends.
func (o *orderedWriter) admit(ctx context.Context, off int64) error {
	for {
		o.mu.Lock()
		if o.err != nil {
			o.mu.Unlock()
			return o.err
		}
		if off < o.next+o.window {
			o.mu.Unlock()
			return nil
		}
		moved := o.moved
		o.mu.Unlock()
		select {
		case <-moved:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// WriteAt writes p if it is next in line, or holds a copy of it until it is.
// The destination is written outside the lock, by one caller at a time: the
// one whose bytes are at the front, which then drains whatever that makes
// contiguous before letting go.
func (o *orderedWriter) WriteAt(p []byte, off int64) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return 0, o.err
	}
	if off != o.next || o.writing {
		o.held[off] = bytes.Clone(p)
		return len(p), nil
	}
	o.writing = true
	defer func() { o.writing = false }()
	for chunk, own := p, true; ; own = false {
		o.mu.Unlock()
		_, err := o.w.Write(chunk)
		o.mu.Lock()
		if err != nil {
			o.err = err
			close(o.moved)
			if own {
				return 0, err
			}
			// p itself landed; the failure belongs to a range that has
			// already been told otherwise, and every call from now on hears
			// about it.
			return len(p), nil
		}
		o.next += int64(len(chunk))
		close(o.moved)
		o.moved = make(chan struct{})
		var ok bool
		if chunk, ok = o.held[o.next]; !ok {
			return len(p), nil
		}
		delete(o.held, o.next)
	}
}

// written is how many bytes have reached the destination.
func (o *orderedWriter) written() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.next
}

// failure is the destination's own error, if it refused a write.
func (o *orderedWriter) failure() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}
