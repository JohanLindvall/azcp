// SPDX-License-Identifier: MIT

//go:build linux || darwin

package local

import "io/fs"

func usageOf(path string, fi fs.FileInfo) (Usage, error) {
	if fi == nil {
		return Usage{}, nil
	}
	allocated, allocationKnown := allocatedBytes(fi)
	id, links, identityKnown := fileIdentity(path, fi)
	return Usage{Allocated: allocated, AllocationKnown: allocationKnown,
		ID: id, Links: uint64(max(0, links)), IdentityKnown: identityKnown}, nil
}
