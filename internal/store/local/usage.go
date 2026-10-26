// SPDX-License-Identifier: MIT

package local

import "io/fs"

// Usage describes an entry itself, without following symbolic links. Allocation
// is distinct from file length: sparse and compressed files can use much less.
type Usage struct {
	Allocated       int64
	AllocationKnown bool
	ID              FileID
	Links           uint64
	IdentityKnown   bool
}

// UsageOf reuses the listing's stat data where possible. On Windows, allocation
// and identity require opening the entry; callers opt in to that extra work.
func UsageOf(path string, fi fs.FileInfo) (Usage, error) { return usageOf(path, fi) }
