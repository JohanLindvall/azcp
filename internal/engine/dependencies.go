package engine

import (
	"context"
	"slices"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
)

// These maps belong to the scanner. Workers only close their task's done
// channel. Waiting on earlier tasks preserves cp's operand order when names
// alias one file, without serialising copies of independent files.
func (e *Engine) awaitDestination(ctx context.Context, id local.FileID) (bool, error) {
	done := e.existingDests[id]
	if done == nil {
		return false, nil
	}
	return true, awaitTask(ctx, done)
}

func awaitTask(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refreshLocalSource catches a source changed by a preceding operand. Source
// nodes may have been collected before any work began, so both reading the
// data and decisions based on its size or timestamp must wait for that write.
func (e *Engine) refreshLocalSource(ctx context.Context, src *store.Node) (*store.Node, error) {
	if src.URL.IsRemote() {
		return src, nil
	}
	waited := false
	if prev := e.scheduled[destinationKey(src.URL)]; prev.done != nil {
		if err := awaitTask(ctx, prev.done); err != nil {
			return nil, err
		}
		waited = true
	}
	if len(e.existingDests) > 0 && src.Info != nil && src.Info.Mode().IsRegular() {
		if id, _, ok := local.IDOf(src.URL.Path, src.Info); ok {
			w, err := e.awaitDestination(ctx, id)
			if err != nil {
				return nil, err
			}
			waited = waited || w
		}
	}
	if waited {
		return e.local.Stat(ctx, src.URL, !src.IsSymlink())
	}
	return src, nil
}

func (e *Engine) awaitLocalAccess(ctx context.Context, t *task) error {
	if t.destID == nil {
		return nil
	}
	if _, err := e.awaitDestination(ctx, *t.destID); err != nil {
		return err
	}
	for _, reader := range e.localReads[*t.destID] {
		if err := awaitTask(ctx, reader); err != nil {
			return err
		}
	}
	delete(e.localReads, *t.destID)
	return nil
}

func (e *Engine) recordLocalAccess(t *task) {
	if t.destID != nil {
		if e.existingDests == nil {
			e.existingDests = make(map[local.FileID]<-chan struct{})
		}
		e.existingDests[*t.destID] = t.done
	}
	if t.sourceID == nil {
		return
	}
	if e.localReads == nil {
		e.localReads = make(map[local.FileID][]<-chan struct{})
	}
	e.localReads[*t.sourceID] = append(e.localReads[*t.sourceID], t.done)
	e.localPlans++
	if e.localPlans%1024 != 0 {
		return
	}
	// The live entries are bounded by queued and active tasks. At most 1023
	// completed registrations accumulate between sweeps.
	for id, readers := range e.localReads {
		readers = slices.DeleteFunc(readers, func(done <-chan struct{}) bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		if len(readers) == 0 {
			delete(e.localReads, id)
		} else {
			e.localReads[id] = readers
		}
	}
}
