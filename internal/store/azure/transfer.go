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
	"strconv"
	"strings"
	"time"

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
	return s.withSignIn(ctx, func() error { return s.upload(ctx, srcPath, dst, o) })
}

func (s *Store) upload(ctx context.Context, srcPath string, dst *uri.URL, o TransferOptions) error {
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
	return s.withSignIn(ctx, func() error { return s.download(ctx, src, f, o) })
}

func (s *Store) download(ctx context.Context, src *store.Node, f *os.File, o TransferOptions) error {
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
	var r io.ReadCloser
	err := s.withSignIn(ctx, func() error {
		var e error
		r, e = s.openRead(ctx, src)
		return e
	})
	return r, err
}

func (s *Store) openRead(ctx context.Context, src *store.Node) (io.ReadCloser, error) {
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

// Copy moves a blob to another blob, keeping the data off this host wherever
// the service allows it.
//
// Three server-side routes are tried in turn, because which one works depends
// on what the endpoint implements and on how the source can be authorised:
//
//  1. Put Blob From URL — one request, for blobs up to 256 MiB.
//  2. Put Block From URL — the same idea block by block, for larger blobs.
//     Both of these can present an OAuth token for the source, so they work
//     across accounts.
//  3. Copy Blob — the asynchronous form. It takes no source-authorisation
//     header, so the source must be readable by the destination account
//     (same account, or carrying a SAS), but it is widely implemented and has
//     no size limit.
//
// Only when none of those works do the bytes pass through this process.
func (s *Store) Copy(ctx context.Context, src *store.Node, dst *uri.URL, o TransferOptions) error {
	if !s.copyRouteDisabled(dst, routeSync) {
		srcURL, auth, err := s.copySource(ctx, src.URL, dst)
		if err != nil {
			s.log.Debug("cannot authorise the source for a server-side copy",
				"source", src.URL.Display(), "reason", err)
		} else {
			err := s.serverCopy(ctx, src, srcURL, auth, dst, o)
			if err == nil {
				s.log.Debug("copied server-side", "route", "put-from-url",
					"source", src.URL.Display(), "destination", dst.Display())
				return nil
			}
			if isCancellation(err) {
				return err
			}
			s.noteCopyFailure(dst, routeSync, src.URL, err)
		}
	}

	if s.asyncCopyViable(src.URL, dst) && !s.copyRouteDisabled(dst, routeAsync) {
		err := s.asyncCopy(ctx, src, dst, o)
		if err == nil {
			s.log.Debug("copied server-side", "route", "copy-blob",
				"source", src.URL.Display(), "destination", dst.Display())
			return nil
		}
		if isCancellation(err) {
			return err
		}
		s.noteCopyFailure(dst, routeAsync, src.URL, err)
	}

	return s.streamCopy(ctx, src, dst, o)
}

// copyRoute names one of the server-side copy mechanisms.
type copyRoute string

const (
	routeSync  copyRoute = "put-from-url"
	routeAsync copyRoute = "copy-blob"
)

// copyRouteDisabled reports whether a route has already been found missing on
// this endpoint. The answer is remembered per endpoint rather than globally,
// because a run can span accounts served by different implementations.
func (s *Store) copyRouteDisabled(dst *uri.URL, route copyRoute) bool {
	_, off := s.noCopyRoute.Load(dst.ServiceURL() + "|" + string(route))
	return off
}

// noteCopyFailure records why a server-side route did not work. A service that
// answers "not implemented" is stating a fact about itself, so that route is
// not tried again against the same endpoint; anything else is treated as a
// per-request problem and only reported.
func (s *Store) noteCopyFailure(dst *uri.URL, route copyRoute, src *uri.URL, err error) {
	if unsupportedByEndpoint(err) {
		key := dst.ServiceURL() + "|" + string(route)
		if _, loaded := s.noCopyRoute.LoadOrStore(key, true); !loaded {
			s.log.Info("endpoint does not implement this server-side copy; "+
				"trying another route",
				"route", string(route), "endpoint", dst.ServiceURL(),
				"cause", retryx.Describe(err))
		}
		return
	}
	s.log.Warn("server-side copy failed, trying another route",
		"route", string(route), "source", src.Display(),
		"destination", dst.Display(), "cause", retryx.Describe(err))
	s.log.Debug("server-side copy failure detail", "route", string(route), "error", err)
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// asyncCopyViable reports whether the destination account could read the source
// on its own. Copy Blob carries no credential for the source, so anything
// needing one has to take a different route.
func (s *Store) asyncCopyViable(src, dst *uri.URL) bool {
	if src.SAS != "" || lookupStatic(src.Account).sas != "" {
		return true
	}
	return src.SameAccount(dst)
}

// asyncCopy asks the service to copy the blob and waits for it to finish.
func (s *Store) asyncCopy(ctx context.Context, src *store.Node, dst *uri.URL, o TransferOptions) error {
	c, err := s.client(ctx, dst)
	if err != nil {
		return err
	}
	srcURL := src.URL.ServiceURL() + "/" + src.URL.Container + "/" + src.URL.Key
	if src.URL.SAS != "" {
		srcURL += "?" + src.URL.SAS
	} else if sas := lookupStatic(src.URL.Account).sas; sas != "" {
		srcURL += "?" + sas
	}

	dstBlob := c.ServiceClient().NewContainerClient(dst.Container).NewBlobClient(dst.Key)
	started, err := dstBlob.StartCopyFromURL(ctx, srcURL, &blob.StartCopyFromURLOptions{
		AccessConditions: o.accessConditions(),
		Tier:             o.tier(),
	})
	if err != nil {
		return err
	}
	if started.CopyStatus != nil && *started.CopyStatus == blob.CopyStatusTypeSuccess {
		if o.Progress != nil {
			o.Progress(src.Size)
		}
		return nil
	}

	copyID := ""
	if started.CopyID != nil {
		copyID = *started.CopyID
	}
	return s.awaitCopy(ctx, dstBlob, copyID, src, dst, o)
}

// awaitCopy polls until the service reports the copy finished. Polling starts
// quickly, since most copies within a region complete almost at once, and backs
// off so a large cross-region copy does not generate needless traffic.
func (s *Store) awaitCopy(ctx context.Context, dstBlob *blob.Client, copyID string,
	src *store.Node, dst *uri.URL, o TransferOptions) error {

	const (
		firstPoll = 100 * time.Millisecond
		maxPoll   = 5 * time.Second
	)
	delay := firstPoll
	for {
		select {
		case <-ctx.Done():
			s.abandonCopy(dstBlob, copyID, dst)
			return ctx.Err()
		case <-time.After(delay):
		}

		props, err := dstBlob.GetProperties(ctx, nil)
		if err != nil {
			return err
		}
		status := blob.CopyStatusTypePending
		if props.CopyStatus != nil {
			status = *props.CopyStatus
		}
		switch status {
		case blob.CopyStatusTypeSuccess:
			if o.Progress != nil {
				o.Progress(src.Size)
			}
			return nil
		case blob.CopyStatusTypeFailed:
			detail := ""
			if props.CopyStatusDescription != nil {
				detail = ": " + *props.CopyStatusDescription
			}
			return fmt.Errorf("the service reported the copy failed%s", detail)
		case blob.CopyStatusTypeAborted:
			return errors.New("the service reported the copy was aborted")
		}
		if o.Progress != nil && props.CopyProgress != nil {
			if done, ok := parseCopyProgress(*props.CopyProgress); ok {
				o.Progress(done)
			}
		}
		delay = min(delay*2, maxPoll)
	}
}

// abandonCopy tells the service to stop a copy this process is no longer
// waiting for. It runs on a fresh deadline because the caller's context is
// already done, and a failure here is not worth reporting as an error: the
// copy the user cancelled is going to be discarded either way.
func (s *Store) abandonCopy(dstBlob *blob.Client, copyID string, dst *uri.URL) {
	if copyID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	if _, err := dstBlob.AbortCopyFromURL(ctx, copyID, nil); err != nil {
		s.log.Debug("could not abort the server-side copy",
			"destination", dst.Display(), "copy_id", copyID, "error", err)
	}
}

// parseCopyProgress reads the "bytesCopied/totalBytes" form the service reports.
func parseCopyProgress(s string) (int64, bool) {
	slash := strings.IndexByte(s, '/')
	if slash <= 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:slash], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
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
		"UnsupportedHeader", "UnsupportedQueryParameter":
		return true
	}
	return false
}

// copySource builds a URL the storage service can read the source from, plus
// the value for the copy-source authorisation header if one is needed.
//
// Which of these applies depends on how the source is authenticated. Failing
// here is not fatal: the caller tries another route.
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
