// SPDX-License-Identifier: MIT

//go:build windows

package local

import (
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

func usageOf(path string, _ fs.FileInfo) (Usage, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Usage{}, err
	}
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return Usage{}, err
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return Usage{}, err
	}
	u := Usage{ID: FileID{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)},
		Links: uint64(info.NumberOfLinks), IdentityKnown: true}
	// FILE_COMPRESSION_INFO reports the allocation of sparse/compressed files
	// through the handle, so a reparse point cannot redirect this lookup.
	var compression struct {
		Size                                int64
		Format                              uint16
		UnitShift, ChunkShift, ClusterShift byte
		Reserved                            [3]byte
	}
	err = windows.GetFileInformationByHandleEx(h, windows.FileCompressionInfo,
		(*byte)(unsafe.Pointer(&compression)), uint32(unsafe.Sizeof(compression)))
	if err == nil {
		u.Allocated, u.AllocationKnown = compression.Size, true
		return u, nil
	}
	// Some filesystems do not support the compression query. Standard allocation
	// remains useful for ordinary files, but must not stand in for a sparse one.
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_SPARSE_FILE|windows.FILE_ATTRIBUTE_COMPRESSED) != 0 {
		return u, nil
	}
	var standard struct {
		Allocation, EndOfFile    int64
		Links                    uint32
		DeletePending, Directory byte
	}
	if err := windows.GetFileInformationByHandleEx(h, windows.FileStandardInfo,
		(*byte)(unsafe.Pointer(&standard)), uint32(unsafe.Sizeof(standard))); err == nil {
		u.Allocated, u.AllocationKnown = standard.Allocation, true
	}
	return u, nil
}
