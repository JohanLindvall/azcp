package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/JohanLindvall/azcp/internal/codec"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store/azure"
)

// A fifo named on the command line is read the way cp reads one: to its end,
// as a stream. What it holds exists only as it passes, so nothing about it can
// be done twice — not a retry, not a resumed run, not a second read to take a
// checksum — and its length is known only once it stops. A pipe or a device
// as the destination is the mirror: written front to back, once, and never
// read back. A copy between two local files needs nothing more than reading
// to the end and not seeking, which local.CopyFile already does; the routes
// to and from blob storage are here.

// uploadPipe sends what a fifo delivers to a blob, by the route a compressed
// upload takes: the one that needs no length in advance.
func (e *Engine) uploadPipe(ctx context.Context, t *task, pt *progress.Task,
	opts azure.TransferOptions, compress bool) error {

	f, err := os.Open(t.src.URL.Path)
	if err != nil {
		return destError(t, err)
	}
	defer f.Close()
	src := &meter{Reader: &contextReader{ctx: ctx, Reader: f}, report: pt.Set}
	var stream io.ReadCloser = io.NopCloser(src)
	if compress {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(e.encode(pw, src)) }()
		// An upload that gives up early must not leave the encoder blocked
		// on a pipe nobody reads.
		defer pr.Close()
		stream = pr
	}
	if err := e.az.UploadEncoded(ctx, readOnce(stream, t.src.URL.Display()), -1, t.dst, opts); err != nil {
		return writeError(t, err)
	}
	pt.SetSize(src.n)
	t.src.Size = src.n
	return nil
}

// readOnce hands out r the first time it is asked and an error after that.
// UploadEncoded starts the stream afresh for each attempt, and what a pipe
// delivered to the first is gone; a second attempt would send whatever was
// left as if it were the whole.
func readOnce(r io.ReadCloser, display string) func() io.ReadCloser {
	used := false
	return func() io.ReadCloser {
		if used {
			return io.NopCloser(failedReader{fmt.Errorf("cannot read %s again: a pipe delivers its contents once",
				quote(display))})
		}
		used = true
		return r
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }

// downloadStream writes a blob into a pipe or a device. With --decompress the
// bytes are expanded on their way through rather than in place afterwards,
// since there is no afterwards to go back to.
func (e *Engine) downloadStream(ctx context.Context, t *task, f *os.File, pt *progress.Task) error {
	opts := e.transferOptions()
	opts.Progress = pt.Set
	if !e.opt.Decompress || !decompressible(t.src.ContentEncoding) {
		return writeError(t, e.az.DownloadTo(ctx, t.src, f, opts))
	}
	pr, pw := io.Pipe()
	expanded := make(chan error, 1)
	go func() {
		err := decodeTo(f, pr, t.src.ContentEncoding, t.src.URL.Display())
		// A decoder that gives up stops the download, rather than leaving it
		// writing into a pipe that nobody reads.
		pr.CloseWithError(err)
		expanded <- err
	}()
	err := e.az.DownloadTo(ctx, t.src, pw, opts)
	pw.CloseWithError(err)
	if xerr := <-expanded; err == nil {
		err = xerr
	}
	return writeError(t, err)
}

// decodeTo writes to w what r decodes to. A write that fails is returned as it
// is, so the destination can be blamed for it; anything else is the data's
// fault, and says so.
func decodeTo(w io.Writer, r io.Reader, encoding, display string) error {
	dec, err := codec.NewReader(r, encoding)
	if err != nil {
		return fmt.Errorf("cannot decompress %s: %w", quote(display), err)
	}
	defer dec.Close()
	buf := make([]byte, 256<<10)
	for {
		n, rerr := dec.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("cannot decompress %s: %w", quote(display), rerr)
		}
	}
}
