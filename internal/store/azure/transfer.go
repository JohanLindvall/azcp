package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"github.com/JohanLindvall/azcp/internal/retryx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Service limits that shape how a large blob has to be assembled.
const (
	maxBlocksPerBlob   = 50_000
	maxSyncCopyBytes   = 256 << 20 // Put Blob From URL takes at most this much
	defaultBlockSize   = 8 << 20
	minBlockSize       = 64 << 10
	maxBlockSizeBytes  = 4000 << 20
	defaultConcurrency = 4
)

// TransferOptions configures one file's worth of data movement.
type TransferOptions struct {
	// BlockSize is the unit of parallel transfer. It is raised automatically
	// when the file is too large to fit in the block-count limit.
	BlockSize int64
	// Concurrency is how many blocks of this one file move at once.
	Concurrency int
	// Progress receives the cumulative byte count. The SDK may report a lower
	// figure than before when it retries a block, so callers must treat the
	// value as absolute rather than as an increment.
	Progress func(transferred int64)
	// ContentType overrides the type guessed from the file extension.
	ContentType string
	// AccessTier sets the blob tier on write.
	AccessTier string
	// NoClobber makes the write fail if the destination blob already exists,
	// closing the gap between checking and writing that a plain stat leaves.
	NoClobber bool
}

func (o TransferOptions) blockSize(size int64) int64 {
	bs := o.BlockSize
	if bs <= 0 {
		bs = defaultBlockSize
	}
	bs = max(bs, minBlockSize)
	// A blob is capped at 50,000 blocks, so very large files force a larger
	// block regardless of what was configured.
	if size > 0 {
		need := (size + maxBlocksPerBlob - 1) / maxBlocksPerBlob
		bs = max(bs, need)
	}
	return min(bs, maxBlockSizeBytes)
}

func (o TransferOptions) concurrency() int {
	if o.Concurrency <= 0 {
		return defaultConcurrency
	}
	return o.Concurrency
}

func (o TransferOptions) httpHeaders(name string) *blob.HTTPHeaders {
	ct := o.ContentType
	if ct == "" {
		ct = guessContentType(name)
	}
	if ct == "" {
		return nil
	}
	return &blob.HTTPHeaders{BlobContentType: &ct}
}

func (o TransferOptions) accessConditions() *blob.AccessConditions {
	if !o.NoClobber {
		return nil
	}
	return &blob.AccessConditions{
		ModifiedAccessConditions: &blob.ModifiedAccessConditions{
			IfNoneMatch: to.Ptr(azcore.ETagAny),
		},
	}
}

func (o TransferOptions) tier() *blob.AccessTier {
	if o.AccessTier == "" {
		return nil
	}
	return to.Ptr(blob.AccessTier(o.AccessTier))
}

// guessContentType maps a file extension to a MIME type. Blob storage defaults
// to application/octet-stream, which makes anything served straight from the
// container download instead of render, so it is worth setting.
func guessContentType(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".log", ".md", ".yaml", ".yml", ".toml", ".ini", ".cfg":
		return "text/plain; charset=utf-8"
	case ".gz", ".tgz":
		return "application/gzip"
	case ".zst":
		return "application/zstd"
	}
	return ""
}

// Upload writes a local file to a blob, staging blocks in parallel.
func (s *Store) Upload(ctx context.Context, srcPath string, dst *uri.URL, o TransferOptions) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	c, err := s.client(ctx, dst)
	if err != nil {
		return err
	}
	_, err = c.UploadFile(ctx, dst.Container, dst.Key, f, &blockblob.UploadFileOptions{
		BlockSize:        o.blockSize(fi.Size()),
		Concurrency:      uint16(o.concurrency()),
		Progress:         o.Progress,
		HTTPHeaders:      o.httpHeaders(srcPath),
		AccessConditions: o.accessConditions(),
		AccessTier:       o.tier(),
	})
	if err != nil {
		return err
	}
	return nil
}

// UploadStream writes an arbitrary reader to a blob. It is used when the source
// size is not known ahead of time, such as when reading from a pipe.
func (s *Store) UploadStream(ctx context.Context, r io.Reader, dst *uri.URL, o TransferOptions) error {
	c, err := s.client(ctx, dst)
	if err != nil {
		return err
	}
	_, err = c.UploadStream(ctx, dst.Container, dst.Key, r, &blockblob.UploadStreamOptions{
		BlockSize:        o.blockSize(0),
		Concurrency:      o.concurrency(),
		HTTPHeaders:      o.httpHeaders(dst.Key),
		AccessConditions: o.accessConditions(),
		AccessTier:       o.tier(),
	})
	if err != nil {
		return err
	}
	return nil
}

// Download writes a blob to an already-open local file, fetching ranges in
// parallel. The file is truncated to the blob's length.
func (s *Store) Download(ctx context.Context, src *store.Node, f *os.File, o TransferOptions) error {
	c, err := s.client(ctx, src.URL)
	if err != nil {
		return err
	}
	if src.Size == 0 {
		// A zero-length range means "to the end", so an empty blob has to be
		// handled here rather than being asked for.
		if err := f.Truncate(0); err != nil {
			return err
		}
		if o.Progress != nil {
			o.Progress(0)
		}
		return nil
	}
	_, err = c.DownloadFile(ctx, src.URL.Container, src.URL.Key, f, &blob.DownloadFileOptions{
		// Passing the known length skips the extra properties request the SDK
		// would otherwise make to discover it.
		Range:       blob.HTTPRange{Offset: 0, Count: src.Size},
		BlockSize:   o.blockSize(src.Size),
		Concurrency: uint16(o.concurrency()),
		Progress:    o.Progress,
		RetryReaderOptionsPerBlock: blob.RetryReaderOptions{
			MaxRetries: s.cfg.MaxRetries,
			OnFailedRead: func(failures int32, lastErr error, rnge blob.HTTPRange, willRetry bool) {
				s.log.Warn("range read failed",
					"blob", src.URL.Display(), "offset", rnge.Offset, "count", rnge.Count,
					"failures", failures, "will_retry", willRetry, "error", lastErr)
			},
		},
	})
	if err != nil {
		return err
	}
	return nil
}

// OpenRead returns a reader over a blob that transparently re-issues the range
// request if the connection drops mid-stream.
func (s *Store) OpenRead(ctx context.Context, src *store.Node) (io.ReadCloser, error) {
	c, err := s.client(ctx, src.URL)
	if err != nil {
		return nil, err
	}
	resp, err := c.DownloadStream(ctx, src.URL.Container, src.URL.Key, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, notExist(src.URL, err)
		}
		return nil, fmt.Errorf("read %s: %w", src.URL.Display(), err)
	}
	return resp.NewRetryReader(ctx, &blob.RetryReaderOptions{
		MaxRetries: s.cfg.MaxRetries,
		OnFailedRead: func(failures int32, lastErr error, rnge blob.HTTPRange, willRetry bool) {
			s.log.Warn("stream read failed",
				"blob", src.URL.Display(), "offset", rnge.Offset,
				"failures", failures, "will_retry", willRetry, "error", lastErr)
		},
	}), nil
}

// Copy moves a blob to another blob. It first asks the service to fetch the
// bytes itself, which keeps the data off this machine entirely; if that is not
// permitted — typically because the source cannot be authorised for the
// destination account — it falls back to streaming through.
func (s *Store) Copy(ctx context.Context, src *store.Node, dst *uri.URL, o TransferOptions) error {
	if s.noServerCopy.Load() {
		return s.streamCopy(ctx, src, dst, o)
	}
	srcURL, auth, err := s.copySource(ctx, src.URL, dst)
	if err != nil {
		s.log.Debug("server-side copy unavailable, streaming instead",
			"source", src.URL.Display(), "reason", err)
		return s.streamCopy(ctx, src, dst, o)
	}

	err = s.serverCopy(ctx, src, srcURL, auth, dst, o)
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if unsupportedByEndpoint(err) {
		// A capability difference, not a fault: say so once and stop asking.
		if s.noServerCopy.CompareAndSwap(false, true) {
			s.log.Info("endpoint does not support server-side copy; "+
				"blob-to-blob copies will stream through this host",
				"endpoint", dst.ServiceURL(), "cause", retryx.Describe(err))
		}
	} else {
		s.log.Warn("server-side copy failed, streaming through this host instead",
			"source", src.URL.Display(), "destination", dst.Display(),
			"cause", retryx.Describe(err))
		s.log.Debug("server-side copy failure detail", "error", err)
	}
	return s.streamCopy(ctx, src, dst, o)
}

// unsupportedByEndpoint reports whether the service said it cannot do this at
// all, as opposed to refusing this particular request. Emulators and older
// endpoints answer this way for the copy-from-URL operations.
func unsupportedByEndpoint(err error) bool {
	var respErr *azcore.ResponseError
	if !errors.As(err, &respErr) {
		return false
	}
	if respErr.StatusCode == http.StatusNotImplemented {
		return true
	}
	switch respErr.ErrorCode {
	case "APINotImplemented", "UnsupportedHttpVerb", "FeatureVersionMismatch",
		"UnsupportedHeader", "InvalidHeaderValue", "UnsupportedQueryParameter":
		return true
	}
	return false
}

// copySource builds a URL the storage service can read the source from, plus
// the value for the copy-source authorisation header if one is needed.
//
// Which of these applies depends on how the source is authenticated, and
// getting it wrong is not fatal: the caller streams the bytes through instead.
func (s *Store) copySource(ctx context.Context, src, dst *uri.URL) (string, *string, error) {
	base := src.ServiceURL() + "/" + src.Container + "/" + src.Key
	if src.SAS != "" {
		return base + "?" + src.SAS, nil, nil
	}
	static := lookupStatic(src.Account)
	if static.sas != "" {
		return base + "?" + static.sas, nil, nil
	}
	if static.accountKey != "" || static.connectionString != "" {
		// With a shared key there is no bearer token to hand over. The service
		// will still read a source in the same account, because the request is
		// signed with that account's key; across accounts it cannot.
		if src.SameAccount(dst) {
			return base, nil, nil
		}
		return "", nil, errors.New("source account is authenticated with a shared key, " +
			"which the destination account cannot present")
	}
	token, err := s.creds.Token(ctx)
	if err != nil {
		return "", nil, err
	}
	return base, to.Ptr("Bearer " + token), nil
}

func (s *Store) serverCopy(ctx context.Context, src *store.Node, srcURL string,
	auth *string, dst *uri.URL, o TransferOptions) error {

	c, err := s.client(ctx, dst)
	if err != nil {
		return err
	}
	bb := c.ServiceClient().NewContainerClient(dst.Container).NewBlockBlobClient(dst.Key)

	if src.Size <= maxSyncCopyBytes {
		_, err := bb.UploadBlobFromURL(ctx, srcURL, &blockblob.UploadBlobFromURLOptions{
			CopySourceAuthorization: auth,
			AccessConditions:        o.accessConditions(),
			Tier:                    o.tier(),
			HTTPHeaders:             o.httpHeaders(dst.Key),
		})
		if err != nil {
			return err
		}
		if o.Progress != nil {
			o.Progress(src.Size)
		}
		return nil
	}

	// Too large for one request: stage it a block at a time, still without the
	// bytes passing through this process.
	bs := o.blockSize(src.Size)
	var ids []string
	var done int64
	for off := int64(0); off < src.Size; off += bs {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(bs, src.Size-off)
		id := blockID(len(ids))
		_, err := bb.StageBlockFromURL(ctx, id, srcURL, &blockblob.StageBlockFromURLOptions{
			CopySourceAuthorization: auth,
			Range:                   blob.HTTPRange{Offset: off, Count: count},
		})
		if err != nil {
			return fmt.Errorf("stage block at offset %d: %w", off, err)
		}
		ids = append(ids, id)
		done += count
		if o.Progress != nil {
			o.Progress(done)
		}
	}
	_, err = bb.CommitBlockList(ctx, ids, &blockblob.CommitBlockListOptions{
		AccessConditions: o.accessConditions(),
		Tier:             o.tier(),
		HTTPHeaders:      o.httpHeaders(dst.Key),
	})
	if err != nil {
		return fmt.Errorf("commit block list: %w", err)
	}
	return nil
}

// streamCopy pulls the source down and pushes it back up, used when the service
// cannot be asked to do the copy itself.
func (s *Store) streamCopy(ctx context.Context, src *store.Node, dst *uri.URL, o TransferOptions) error {
	r, err := s.OpenRead(ctx, src)
	if err != nil {
		return err
	}
	defer r.Close()

	var seen int64
	counted := io.Reader(r)
	if o.Progress != nil {
		counted = &countingReader{r: r, total: &seen, report: o.Progress}
	}
	c, err := s.client(ctx, dst)
	if err != nil {
		return err
	}
	_, err = c.UploadStream(ctx, dst.Container, dst.Key, counted, &blockblob.UploadStreamOptions{
		BlockSize:        o.blockSize(src.Size),
		Concurrency:      o.concurrency(),
		HTTPHeaders:      o.httpHeaders(dst.Key),
		AccessConditions: o.accessConditions(),
		AccessTier:       o.tier(),
	})
	if err != nil {
		return err
	}
	return nil
}

// countingReader reports cumulative bytes pulled from the source, which for a
// stream-through copy is the honest measure of progress.
type countingReader struct {
	r      io.Reader
	total  *int64
	report func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		*c.total += int64(n)
		c.report(*c.total)
	}
	return n, err
}

// blockID renders a block index as the fixed-width, base64 identifier the
// service requires. Every block in one blob must encode to the same length.
func blockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("azcp-blk-%08d", i)))
}
