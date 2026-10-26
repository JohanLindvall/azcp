// SPDX-License-Identifier: MIT

package progress

import "time"

// Snapshot is a read-only view for displays which own their own terminal.
// Totals remain provisional while Scanning is true.
type Snapshot struct {
	Phase, Current                        string
	Scanning                              bool
	Active                                int
	Done, Failed, Skipped, Retries, Bytes int64
	PlannedFiles, PlannedBytes            int64
	Elapsed                               time.Duration
}

// Snapshot copies state without taking the paint lock or doing terminal I/O.
// Workers never wait for a consumer to render the returned values.
func (r *Reporter) Snapshot() Snapshot {
	r.mu.Lock()
	s := Snapshot{Phase: r.phase, Scanning: r.scanning, Active: len(r.active)}
	if len(r.active) > 0 {
		s.Current = r.active[0].name
	}
	r.mu.Unlock()
	s.Done, s.Failed, s.Skipped, s.Retries, s.Bytes, s.Elapsed = r.Totals()
	s.PlannedFiles, s.PlannedBytes = r.plannedFiles.Load(), r.plannedBytes.Load()
	return s
}
