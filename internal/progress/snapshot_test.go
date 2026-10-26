// SPDX-License-Identifier: MIT

package progress

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSnapshotTracksScanningCorrectionsAndFailures(t *testing.T) {
	r := newQuietReporter()
	defer r.Stop()
	r.SetScanning(true)
	r.Plan(2, 200)
	task := r.Begin("file", 100, DirUpload)
	task.Set(60)
	task.Retrying(2, 3, time.Second)
	s := r.Snapshot()
	if !s.Scanning || s.Active != 1 || s.Current != "file" || s.Bytes != 60 || s.PlannedFiles != 2 || s.PlannedBytes != 200 || s.Retries != 1 {
		t.Fatalf("scan snapshot: %+v", s)
	}
	r.SetScanning(false)
	task.SetSize(150)
	task.Done(nil)
	failed := r.Begin("failed", 100, DirUpload)
	failed.Set(30)
	failed.Done(errors.New("refused"))
	s = r.Snapshot()
	if s.Scanning || s.Active != 0 || s.Current != "" || s.Bytes != 150 || s.PlannedBytes != 250 || s.Done != 1 || s.Failed != 1 {
		t.Fatalf("completed snapshot: %+v", s)
	}
}

func TestSnapshotsDoNotNeedTerminalLock(t *testing.T) {
	r := newQuietReporter()
	defer r.Stop()
	// A blocked terminal cannot block either a worker or a snapshot consumer.
	r.paint.Lock()
	defer r.paint.Unlock()
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 25 {
				r.Plan(1, 100)
				task := r.Begin("file", 100, DirLocal)
				task.Add(50)
				_ = r.Snapshot()
				task.Done(nil)
			}
		})
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal lock blocked workers or snapshots")
	}
	s := r.Snapshot()
	if s.Done != 400 || s.PlannedFiles != 400 || s.Bytes != 40000 || s.PlannedBytes != 40000 || s.Active != 0 {
		t.Fatalf("parallel snapshots lost accounting: %+v", s)
	}
}
