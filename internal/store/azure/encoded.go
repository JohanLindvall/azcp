package azure

import (
	"bytes"
	"context"
	"crypto/md5"
	"io"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"github.com/JohanLindvall/azcp/internal/uri"
)

// UploadEncoded writes a stream whose length is known only once it ends — a
// file being compressed on the way — to a blob. open starts the stream, and is
// called afresh for every attempt, since a stream cannot be rewound. sourceSize
// is what the stream is made from and picks the route: what came from one
// block's worth of source is gathered and written in a single request, with
// its checksum; anything larger is staged block by block as it arrives.
//
// This is the one place the SDK's own stream upload is used. It cannot resume
// — the blocks it stages are named as it goes — so an interrupted compressed
// upload starts over, and the caller's progress is measured on the source,
// which is the only length anybody knows in advance.
func (s *Store) UploadEncoded(ctx context.Context, open func() io.ReadCloser, sourceSize int64,
	dst *uri.URL, o TransferOptions) error {

	return s.withSignIn(ctx, func() error {
		r := open()
		defer r.Close()
		return s.uploadEncoded(ctx, r, sourceSize, dst, o)
	})
}

func (s *Store) uploadEncoded(ctx context.Context, r io.Reader, sourceSize int64,
	dst *uri.URL, o TransferOptions) error {

	bb, err := s.blockBlobClient(ctx, dst)
	if err != nil {
		return err
	}
	sum := md5.New()
	if o.PutMD5 {
		// Sequential, unlike a file's blocks, so the hash rides along.
		r = io.TeeReader(r, sum)
	}

	// The SDK's stream upload will not stage blocks smaller than a mebibyte
	// whatever it is asked, and single-shots a stream that fits in one; so
	// anything that would be single-shotted anyway is gathered here instead,
	// where its checksum can go in the same request.
	const streamFloor = 1 << 20
	if sourceSize <= max(o.blockSize(sourceSize), streamFloor) {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			return err
		}
		var digest []byte
		if o.PutMD5 {
			digest = sum.Sum(nil)
		}
		_, err := bb.Upload(ctx, streaming.NopCloser(bytes.NewReader(buf.Bytes())),
			&blockblob.UploadOptions{
				HTTPHeaders:      o.httpHeadersWithMD5(dst.Key, digest),
				Metadata:         o.metadata(),
				AccessConditions: o.accessConditions(),
				Tier:             o.tier(),
			})
		return err
	}

	// The block count is bounded, so the block is sized for the source plus
	// the little a format can add to data it cannot shrink — never for less
	// than the source, where a file just under the limit could go over it.
	_, err = bb.UploadStream(ctx, r, &blockblob.UploadStreamOptions{
		BlockSize:        o.blockSize(sourceSize + sourceSize/8),
		Concurrency:      o.concurrency(),
		HTTPHeaders:      o.httpHeaders(dst.Key),
		Metadata:         o.metadata(),
		AccessConditions: o.accessConditions(),
		AccessTier:       o.tier(),
	})
	if err != nil || !o.PutMD5 {
		return err
	}
	// The checksum is known only now. Set Blob Properties clears every header
	// it is not given, so the ones just written go along with it.
	bc, err := s.blobClient(ctx, dst)
	if err != nil {
		return err
	}
	_, err = bc.SetHTTPHeaders(ctx, *o.httpHeadersWithMD5(dst.Key, sum.Sum(nil)), nil)
	return err
}
