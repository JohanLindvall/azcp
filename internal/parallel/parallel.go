// Package parallel runs indexed work with bounded concurrency and cancels the
// remaining work on failure. Callers retain control of progress and retries.
package parallel

import (
	"context"
	"sync"
	"sync/atomic"
)

// Do calls fn once for each index in [0, count), using at most workers
// goroutines. It waits for all started work and returns the first failure, or
// the caller's cancellation. A non-positive worker count runs serially.
func Do(ctx context.Context, count, workers int, fn func(context.Context, int) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if count <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)
	// Reuse workers across blocks: a resumed large file may have thousands
	// of already completed ranges, each requiring only an index lookup.
	for range min(count, max(1, workers)) {
		wg.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= count {
					return
				}
				if err := fn(ctx, i); err != nil {
					once.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
