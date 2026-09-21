package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestBenchmarkStopsOnFirstFailure(t *testing.T) {
	e := newEngine(t, "--benchmark=10x1KiB", "--jobs=1", "azure://acct/c")
	u := mustURL(t, "azure://acct/c/test")
	boom := errors.New("upload failed")
	ran := 0
	err := e.benchEach(context.Background(), []*uri.URL{u, u, u}, progress.DirUpload,
		func(context.Context, *uri.URL, *progress.Task) error {
			ran++
			return boom
		})
	if !errors.Is(err, boom) || ran != 1 {
		t.Fatalf("error = %v, attempted files = %d", err, ran)
	}
	_, failed, _, _, _, _ := e.prog.Totals()
	if failed != 1 {
		t.Fatalf("failed files = %d", failed)
	}
}

func TestBenchmarkCancellationIsUnfinished(t *testing.T) {
	e := newEngine(t, "--benchmark=10x1KiB", "--jobs=1", "azure://acct/c")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := e.benchEach(ctx, []*uri.URL{mustURL(t, "azure://acct/c/test")}, progress.DirUpload,
		func(ctx context.Context, _ *uri.URL, pt *progress.Task) error {
			pt.Set(100)
			cancel()
			return ctx.Err()
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	done, failed, _, _, bytes, _ := e.prog.Totals()
	up, _ := e.prog.Unfinished()
	if done != 0 || failed != 0 || bytes != 0 || up != 1 {
		t.Fatalf("done=%d, failed=%d, bytes=%d, unfinished=%d", done, failed, bytes, up)
	}
}
