package parallel

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestDoVisitsEachIndexOnce(t *testing.T) {
	for _, workers := range []int{0, 1, 8, 10000} {
		seen := make([]atomic.Int32, 100)
		err := Do(context.Background(), len(seen), workers, func(_ context.Context, i int) error {
			seen[i].Add(1)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := range seen {
			if got := seen[i].Load(); got != 1 {
				t.Fatalf("workers=%d index %d: ran %d times", workers, i, got)
			}
		}
	}
}

func TestDoCancelledContextStartsNoWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Do(ctx, 100, 8, func(context.Context, int) error {
		t.Error("started work after cancellation")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestDoCancelsAndJoinsWorkers(t *testing.T) {
	boom := errors.New("failed")
	started := make(chan struct{}, 3)
	var finished atomic.Int32
	err := Do(context.Background(), 100, 4, func(ctx context.Context, i int) error {
		defer finished.Add(1)
		if i == 0 {
			for range 3 {
				<-started
			}
			return boom
		}
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, boom) || finished.Load() != 4 {
		t.Fatalf("error = %v, finished = %d", err, finished.Load())
	}
}
