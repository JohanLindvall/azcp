package azure

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/klauspost/compress/gzip"
)

// gzipped returns a fresh reader over the gzip form of data on every call, the
// way the engine hands UploadEncoded a stream it can start again.
func gzipped(t *testing.T, data []byte) func() io.ReadCloser {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return func() io.ReadCloser { return io.NopCloser(bytes.NewReader(buf.Bytes())) }
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func md5Header(data []byte) string {
	sum := md5.Sum(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// A source that fits one block is gathered and written in a single request,
// with its checksum, type and encoding all on it from the start.
func TestUploadEncodedSmallSourceIsOneRequest(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "seed", nil, nil)
	s, at := fakeStore(t, f, false)
	src := bytes.Repeat([]byte("the quick brown fox "), 50)
	o := TransferOptions{PutMD5: true, ContentEncoding: "gzip", ContentType: "text/plain"}

	err := s.UploadEncoded(context.Background(), gzipped(t, src), int64(len(src)), at("c/fox.txt.gz"), o)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := f.blob("c", "fox.txt.gz")
	if !ok {
		t.Fatal("nothing was written")
	}
	if !bytes.Equal(gunzip(t, b.data), src) {
		t.Error("the blob does not expand to the source")
	}
	if b.encoding != "gzip" || b.contentType != "text/plain" || b.md5 != md5Header(b.data) {
		t.Errorf("headers: encoding %q, type %q, md5 %q", b.encoding, b.contentType, b.md5)
	}
	if f.count("put blob") != 1 || f.count("stage block") != 0 || f.count("set properties") != 0 {
		t.Errorf("a small source cost %d puts, %d stages, %d property calls",
			f.count("put blob"), f.count("stage block"), f.count("set properties"))
	}
}

// A larger source is staged as it arrives. Its checksum is known only at the
// end, so it follows in one more request — and in none when nobody asked.
func TestUploadEncodedLargeSourceIsStaged(t *testing.T) {
	f := newFakeBlobs()
	f.put("c", "seed", nil, nil)
	s, at := fakeStore(t, f, false)
	// Incompressible, and larger than the smallest block the SDK will stream,
	// so the compressed form spans several blocks.
	src := make([]byte, 3<<20+512)
	if _, err := rand.NewChaCha8([32]byte{1}).Read(src); err != nil {
		t.Fatal(err)
	}
	o := TransferOptions{PutMD5: true, BlockSize: 1 << 20, Concurrency: 2, ContentEncoding: "gzip"}

	err := s.UploadEncoded(context.Background(), gzipped(t, src), int64(len(src)), at("c/noise.bin.gz"), o)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := f.blob("c", "noise.bin.gz")
	if !ok {
		t.Fatal("nothing was written")
	}
	if !bytes.Equal(gunzip(t, b.data), src) {
		t.Error("the blob does not expand to the source")
	}
	if b.encoding != "gzip" || b.md5 != md5Header(b.data) {
		t.Errorf("headers: encoding %q, md5 %q (want %q)", b.encoding, b.md5, md5Header(b.data))
	}
	if f.count("stage block") < 3 || f.count("commit") != 1 || f.count("set properties") != 1 || f.count("put blob") != 0 {
		t.Errorf("a large source cost %d stages, %d commits, %d property calls, %d puts",
			f.count("stage block"), f.count("commit"), f.count("set properties"), f.count("put blob"))
	}

	o.PutMD5 = false
	before := f.count("set properties")
	if err := s.UploadEncoded(context.Background(), gzipped(t, src), int64(len(src)), at("c/quiet.bin.gz"), o); err != nil {
		t.Fatal(err)
	}
	if f.count("set properties") != before {
		t.Error("headers were set again with no checksum to add")
	}
}
