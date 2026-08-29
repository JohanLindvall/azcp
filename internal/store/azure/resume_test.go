package azure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func testNode(t *testing.T, etag string, size int64) *store.Node {
	t.Helper()
	u, err := uri.Parse("azure://acct/c/blob.bin", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &store.Node{URL: u, Kind: store.KindFile, Size: size, ETag: etag}
}

// A record survives the process and hands back exactly the ranges that were
// marked, and only for the blob it was written for.
func TestResumeRecordRoundTrip(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "blob.bin")
	node := testNode(t, `"etag-1"`, 100)

	r, err := openResumeFile(dst, node, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 3, 9} {
		if err := r.mark(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.mark(3); err != nil { // marking twice is fine
		t.Fatal(err)
	}
	r.close()

	r2, err := openResumeFile(dst, node, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.close()
	for _, i := range []int{0, 3, 9} {
		if !r2.has(i) {
			t.Errorf("range %d lost across restart", i)
		}
	}
	if r2.has(1) {
		t.Error("an unmarked range came back marked")
	}
	// Blocks 0 and 3 are full (10 bytes); block 9 is the tail (100-90=10).
	if got := r2.bytesDone(10, 100); got != 30 {
		t.Errorf("bytesDone = %d, want 30", got)
	}
	if !IncompleteDownload(dst) {
		t.Error("an open record does not mark the download incomplete")
	}
}

// A record describing a different blob — another etag, size or block size —
// must be discarded, not continued into.
func TestResumeRecordRejectsChangedBlob(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "blob.bin")
	r, err := openResumeFile(dst, testNode(t, `"etag-1"`, 100), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mark(5); err != nil {
		t.Fatal(err)
	}
	r.close()

	for name, node := range map[string]*store.Node{
		"different etag": testNode(t, `"etag-2"`, 100),
		"different size": testNode(t, `"etag-1"`, 200),
	} {
		r2, err := openResumeFile(dst, node, 10)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r2.has(5) {
			t.Errorf("%s: a stale range was believed", name)
		}
		r2.close()
	}
}

func TestResumeRecordDoneRemovesIt(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "blob.bin")
	r, err := openResumeFile(dst, testNode(t, `"e"`, 10), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.mark(0); err != nil {
		t.Fatal(err)
	}
	r.done()
	if IncompleteDownload(dst) {
		t.Error("done() left the record behind")
	}
	if err := removeResumeRecord(dst); err != nil {
		t.Errorf("removing an absent record should be quiet: %v", err)
	}
}

func TestBlockIDsAreUniformAndDistinct(t *testing.T) {
	a, b := blockID(0), blockID(49_999)
	if len(a) != len(b) {
		t.Errorf("block ids differ in length: %q vs %q", a, b)
	}
	if a == b {
		t.Error("distinct blocks share an id")
	}
}

// inParallel is the scaffolding under uploads, downloads and staged copies:
// every index must run, the first error must win, and a failure must stop the
// work still queued behind it.
func TestInParallelRunsEverything(t *testing.T) {
	var ran atomic.Int64
	err := inParallel(context.Background(), 100, 8, func(_ context.Context, i int) error {
		ran.Add(1)
		return nil
	})
	if err != nil || ran.Load() != 100 {
		t.Fatalf("ran %d, err %v", ran.Load(), err)
	}
}

func TestInParallelStopsAfterFailure(t *testing.T) {
	boom := errors.New("boom")
	var after atomic.Int64
	err := inParallel(context.Background(), 10_000, 4, func(ctx context.Context, i int) error {
		if i == 5 {
			return boom
		}
		if ctx.Err() != nil {
			return ctx.Err() // cancelled work must not outvote the real error
		}
		after.Add(1)
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the first real failure", err)
	}
	if after.Load() > 9_000 {
		t.Errorf("cancellation barely stopped anything (%d ran)", after.Load())
	}
}

func TestInParallelHonoursCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := inParallel(ctx, 10, 2, func(context.Context, int) error { return nil })
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestSafeFileName(t *testing.T) {
	if got := safeFileName("contoso.onmicrosoft.com"); got != "contoso_onmicrosoft_com" {
		t.Errorf("safeFileName = %q", got)
	}
	if got := safeFileName("../../etc"); got != "______etc" {
		t.Errorf("path characters survived: %q", got)
	}
}

func TestParseCopyProgress(t *testing.T) {
	if n, ok := parseCopyProgress("512/1024"); !ok || n != 512 {
		t.Errorf("got %d, %v", n, ok)
	}
	for _, bad := range []string{"", "/1024", "x/y"} {
		if _, ok := parseCopyProgress(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestUnfinishedRecordSurvivesCrashMidWrite(t *testing.T) {
	// A record whose header line is torn (no newline, partial content) must be
	// treated as describing nothing, not trusted.
	dst := filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(dst+resumeSuffix, []byte("azcp-resu"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := openResumeFile(dst, testNode(t, `"e"`, 100), 10)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if r.has(0) {
		t.Error("a torn record was believed")
	}
}
