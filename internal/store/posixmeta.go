// SPDX-License-Identifier: MIT

package store

import (
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// These are AzCopy's standard POSIX metadata keys. Modes include Unix file
// type bits in decimal; timestamps are signed Unix nanoseconds. The statx
// mask prevents consumers from inventing attributes that were not preserved.
const (
	MetaMode      = "permissions"
	MetaUID       = "posix_owner"
	MetaGID       = "posix_group"
	MetaMTime     = "modtime"
	MetaATime     = "posix_atime"
	MetaSymlink   = "is_symlink"
	MetaFolder    = "hdi_isfolder"
	MetaStatxMask = "linux_statx_mask"
)

const (
	statxType  = 0x01
	statxMode  = 0x02
	statxUID   = 0x08
	statxGID   = 0x10
	statxATime = 0x20
	statxMTime = 0x40
)

// PosixMeta holds the filesystem attributes azcp preserves in AzCopy's format.
type PosixMeta struct {
	Mode         fs.FileMode
	HasMode      bool
	UID, GID     int
	HasOwner     bool
	MTime, ATime time.Time
	Symlink      bool
}

func (p PosixMeta) Encode() map[string]string {
	m := make(map[string]string)
	mask := 0
	if p.HasMode {
		kind := uint32(0o100000)
		if p.Mode.IsDir() {
			kind = 0o040000
		}
		if p.Symlink {
			kind = 0o120000
		}
		m[MetaMode] = strconv.FormatUint(uint64(modeBits(p.Mode)|kind), 10)
		mask |= statxType | statxMode
	}
	if p.HasOwner {
		m[MetaUID], m[MetaGID] = strconv.Itoa(p.UID), strconv.Itoa(p.GID)
		mask |= statxUID | statxGID
	}
	if !p.MTime.IsZero() {
		m[MetaMTime] = strconv.FormatInt(p.MTime.UnixNano(), 10)
		mask |= statxMTime
	}
	if !p.ATime.IsZero() {
		m[MetaATime] = strconv.FormatInt(p.ATime.UnixNano(), 10)
		mask |= statxATime
	}
	if p.Symlink {
		m[MetaSymlink] = "true"
	}
	if p.Mode.IsDir() {
		m[MetaFolder] = "true"
	}
	if len(m) == 0 {
		return nil
	}
	m[MetaStatxMask] = strconv.Itoa(mask)
	return m
}

// DecodePosixMeta accepts AzCopy's standard and AMLFS styles. Unreadable or
// masked-out values are ignored. The retired azcp_* keys are not interpreted.
func DecodePosixMeta(m map[string]string) PosixMeta {
	var p PosixMeta
	get := func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		for mk, mv := range m {
			if strings.EqualFold(mk, k) {
				return mv
			}
		}
		return ""
	}
	mask := uint64(0xffff)
	if v := get(MetaStatxMask); v != "" {
		parsed, err := strconv.ParseUint(v, 10, 32)
		mask = 0
		if err == nil {
			mask = parsed
		}
	}
	p.Symlink = strings.EqualFold(get(MetaSymlink), "true")
	if v := get(MetaMode); v != "" && mask&statxMode != 0 {
		base := 10
		if get("owner") != "" || get("group") != "" || len(v) > 1 && v[0] == '0' {
			base = 8
		}
		if bits, err := strconv.ParseUint(v, base, 16); err == nil {
			// A type-less AMLFS mode or a regular file, directory or link.
			// Device/socket/pipe metadata must not become permission bits.
			switch bits & 0o170000 {
			case 0, 0o100000, 0o040000, 0o120000:
				p.Mode, p.HasMode = FileModeFromBits(uint32(bits)), true
			}
		}
	}
	uid, gid := get(MetaUID), get(MetaGID)
	if v := get("owner"); v != "" {
		uid = v
	}
	if v := get("group"); v != "" {
		gid = v
	}
	u, ue := strconv.ParseUint(uid, 10, 32)
	g, ge := strconv.ParseUint(gid, 10, 32)
	if ue == nil && ge == nil && u < 1<<32-1 && g < 1<<32-1 &&
		mask&(statxUID|statxGID) == statxUID|statxGID {
		p.UID, p.GID, p.HasOwner = int(u), int(g), true
	}
	parseTime := func(v string) time.Time {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(0, n).UTC()
		}
		t, _ := time.Parse("2006-01-02 15:04:05 -0700", v)
		return t
	}
	if mask&statxMTime != 0 {
		p.MTime = parseTime(get(MetaMTime))
	}
	// AzCopy records atime even when a noatime mount clears its statx bit.
	p.ATime = parseTime(get(MetaATime))
	return p
}

// modeBits converts Go's FileMode into the traditional twelve bits, so the
// stored value is the one `stat` would show.
func modeBits(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// FileModeFromBits is the inverse of modeBits.
func FileModeFromBits(bits uint32) fs.FileMode {
	m := fs.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// IsSymlink reports AzCopy's explicit symlink marker. The target is blob data.
func (p PosixMeta) IsSymlink() bool { return p.Symlink }
