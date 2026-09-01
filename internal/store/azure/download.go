package azure

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Ranges are fetched here rather than through the SDK's DownloadFile for two
// reasons. The first is a bug: told the blob's size — which saves it a request
// — DownloadFile dereferences a Content-Length the service does not send for a
// blob with a content encoding, and the process dies of a nil pointer. The
// second is that resuming needs to know which ranges have landed, and that is
// only knowable from the code issuing them.

// downloadRanges fetches the blob into f, in parallel, one range per block.
func (s *Store) downloadRanges(ctx context.Context, src *store.Node, f io.WriterAt,
	o TransferOptions, resume *resumeFile) error {

	bc, err := s.blobClient(ctx, src.URL)
	if err != nil {
		return err
	}

	// A real file is sized up front so ranges can be written where they
	// belong; the benchmark's discarding writer has nothing to size.
	if t, ok := f.(interface{ Truncate(int64) error }); ok {
		if err := t.Truncate(src.Size); err != nil {
			return err
		}
	}
	blockSize := o.blockSize(src.Size)
	count := int((src.Size + blockSize - 1) / blockSize)

	var fetched atomic.Int64
	// Ranges already on disk from an earlier run are counted as progress so
	// the display reflects the whole file, not just this attempt's share.
	if resume != nil {
		fetched.Store(resume.bytesDone(blockSize, src.Size))
		if o.Progress != nil {
			o.Progress(fetched.Load())
		}
	}

	return inParallel(ctx, count, o.concurrency(), func(ctx context.Context, i int) error {
		if resume != nil && resume.has(i) {
			return nil
		}
		offset := int64(i) * blockSize
		n := min(blockSize, src.Size-offset)

		resp, err := bc.DownloadStream(ctx, &blob.DownloadStreamOptions{
			Range: blob.HTTPRange{Offset: offset, Count: n},
		})
		if err != nil {
			return fmt.Errorf("reading bytes %d-%d: %w", offset, offset+n-1, err)
		}
		body := resp.NewRetryReader(ctx, &blob.RetryReaderOptions{
			MaxRetries: s.cfg.MaxRetries,
			OnFailedRead: func(failures int32, lastErr error, rng blob.HTTPRange, willRetry bool) {
				if ctx.Err() != nil {
					// The run is being cancelled, which fails every range
					// still in flight at once. One line each turns Ctrl-C
					// into a screenful of things that went wrong.
					return
				}
				s.log.Warn("range read failed",
					"blob", src.URL.Display(), "offset", rng.Offset,
					"failures", failures, "will_retry", willRetry, "error", lastErr)
			},
		})
		written, err := io.Copy(&sectionWriter{w: f, off: offset, limit: n}, body)
		body.Close()
		if err != nil {
			return fmt.Errorf("writing bytes %d-%d: %w", offset, offset+n-1, err)
		}
		if written != n {
			return fmt.Errorf("short read at offset %d: got %d of %d bytes",
				offset, written, n)
		}
		// Recorded only once the bytes are on disk, so a resumed run never
		// trusts a range that was still in flight.
		if resume != nil {
			if err := resume.mark(i); err != nil {
				s.log.Warn("cannot record download progress",
					"blob", src.URL.Display(), "error", err)
			}
		}
		if o.Progress != nil {
			o.Progress(fetched.Add(n))
		}
		return nil
	})
}

// sectionWriter writes into a fixed window of a file, refusing to run past it.
type sectionWriter struct {
	w     io.WriterAt
	off   int64
	limit int64
}

func (s *sectionWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > s.limit {
		return 0, fmt.Errorf("range produced more than the %d bytes expected", s.limit)
	}
	n, err := s.w.WriteAt(p, s.off)
	s.off += int64(n)
	s.limit -= int64(n)
	return n, err
}

// DownloadDiscard reads a blob and throws the bytes away. It exists for the
// benchmark, where writing to disk would measure the disk.
func (s *Store) DownloadDiscard(ctx context.Context, src *store.Node, o TransferOptions) error {
	return s.withSignIn(ctx, func() error {
		return s.downloadRanges(ctx, src, discardAt{}, o, nil)
	})
}

// discardAt satisfies io.WriterAt by accepting everything and keeping none of
// it.
type discardAt struct{}

func (discardAt) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

// blobURL is the plain URL of a blob, used where a message needs one.
func blobURL(u *uri.URL) string {
	return u.ServiceURL() + "/" + u.Container + "/" + u.Key
}
