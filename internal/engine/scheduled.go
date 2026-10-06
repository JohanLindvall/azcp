// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type plannedCopy struct {
	source  string
	done    <-chan struct{}
	sidecar bool
	backup  bool
}

func destinationKey(u *uri.URL) string {
	if u.IsRemote() {
		return u.ServiceURL() + "/" + u.PathPart()
	}
	p, err := filepath.Abs(u.Path)
	if err != nil {
		return u.Path
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func (e *Engine) reserveDestination(ctx context.Context, src *store.Node, dst *uri.URL) (bool, error) {
	if e.scheduled == nil {
		return true, nil
	}
	previous, exists := e.scheduled[destinationKey(dst)]
	if !exists {
		if e.reservesResumeRecord(src, dst) {
			if _, exists := e.scheduled[destinationKey(dst)+azure.ResumeSuffix]; exists {
				return false, plainf("cannot resume %s: its resume record is also a copy destination", quote(dst.Display()))
			}
		}
		return true, nil
	}
	if previous.sidecar {
		return false, plainf("cannot copy to %s: the name is reserved for another download's resume record", quote(dst.Display()))
	}
	if previous.backup {
		// A backup is an ordinary existing destination to the next operand,
		// but its rename must finish before overwrite rules inspect it.
		return true, awaitTask(ctx, previous.done)
	}
	if e.opt.NoClobber || e.opt.Update == cli.UpdateNone {
		return false, nil
	}
	if e.opt.Update == cli.UpdateNoneFail {
		return false, plainf("not replacing %s", quote(dst.Display()))
	}
	if previous.source == destinationKey(src.URL) {
		e.note("warning: source file %s specified more than once", quote(src.URL.Display()))
		return false, nil
	}
	if e.opt.Backup == cli.BackupNone {
		return false, plainf("will not overwrite just-created %s with %s", quote(dst.Display()), quote(src.URL.Display()))
	}
	// Backups intentionally retain both operands. Waiting only at a collision
	// lets the scanner weigh the completed first copy and choose the next
	// backup number without pacing unrelated transfers.
	select {
	case <-previous.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (e *Engine) reservesResumeRecord(src *store.Node, dst *uri.URL) bool {
	return e.opt.Resume && !e.opt.AttributesOnly && !e.streamDest && src.URL.IsRemote() && !dst.IsRemote()
}
