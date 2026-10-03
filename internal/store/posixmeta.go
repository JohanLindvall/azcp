package store

import (
	"encoding/base64"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// Blob storage has no notion of a file mode, an owner or a symbolic link, so a
// filesystem tree copied into it arrives stripped of everything but its bytes
// and its name. What it does have is user metadata, which is enough to carry
// the rest: `azcp -a tree azure://…` and back again returns the tree it started
// with, rather than a flattened approximation of it.
//
// The keys are namespaced so they cannot collide with metadata the user sets,
// and every value is a plain string, because that is all blob metadata holds.
// Anything unreadable is ignored rather than guessed at: a missing mode is
// better than a wrong one.

const (
	// MetaMode is the file mode, in octal, including the setuid, setgid and
	// sticky bits.
	MetaMode = "azcp_mode"
	// MetaUID is the numeric owner.
	MetaUID = "azcp_uid"
	// MetaGID is the numeric group.
	MetaGID = "azcp_gid"
	// MetaMTime is the modification time, RFC 3339 with nanoseconds.
	MetaMTime = "azcp_mtime"
	// MetaATime is the access time, RFC 3339 with nanoseconds.
	MetaATime = "azcp_atime"
	// MetaSymlink is the link target. A blob carrying it is a symbolic link,
	// stored as a zero-length blob because the target is the whole content.
	MetaSymlink = "azcp_symlink"
	// MetaSymlinkBase64 carries targets that cannot survive an ASCII HTTP
	// header, including non-ASCII bytes and leading or trailing whitespace.
	MetaSymlinkBase64 = "azcp_symlink_base64"
)

// PosixMeta is the attribute set that survives a round trip through blob
// storage.
type PosixMeta struct {
	Mode        fs.FileMode
	HasMode     bool
	UID, GID    int
	HasOwner    bool
	MTime       time.Time
	ATime       time.Time
	SymlinkDest string
}

// Encode renders the attributes as blob metadata.
func (p PosixMeta) Encode() map[string]string {
	m := map[string]string{}
	if p.HasMode {
		m[MetaMode] = "0" + strconv.FormatUint(uint64(modeBits(p.Mode)), 8)
	}
	if p.HasOwner {
		m[MetaUID] = strconv.Itoa(p.UID)
		m[MetaGID] = strconv.Itoa(p.GID)
	}
	if !p.MTime.IsZero() {
		m[MetaMTime] = p.MTime.UTC().Format(time.RFC3339Nano)
	}
	if !p.ATime.IsZero() {
		m[MetaATime] = p.ATime.UTC().Format(time.RFC3339Nano)
	}
	if p.SymlinkDest != "" {
		if strings.TrimSpace(p.SymlinkDest) != p.SymlinkDest ||
			strings.ContainsFunc(p.SymlinkDest, func(r rune) bool { return r < 0x20 || r >= 0x7f }) {
			m[MetaSymlinkBase64] = base64.StdEncoding.EncodeToString([]byte(p.SymlinkDest))
		} else {
			m[MetaSymlink] = p.SymlinkDest
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// DecodePosixMeta reads back what Encode wrote. Values that cannot be parsed
// are dropped: restoring nothing is safer than restoring a wrong mode.
func DecodePosixMeta(m map[string]string) PosixMeta {
	var p PosixMeta
	if m == nil {
		return p
	}
	get := func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		// Azure lowercases metadata keys on some paths; the keys here are
		// already lowercase, but a case-insensitive lookup costs nothing and
		// makes the round trip robust.
		for mk, mv := range m {
			if strings.EqualFold(mk, k) {
				return mv
			}
		}
		return ""
	}
	if v := get(MetaMode); v != "" {
		if bits, err := strconv.ParseUint(v, 8, 32); err == nil && bits <= 0o7777 {
			p.Mode, p.HasMode = FileModeFromBits(uint32(bits)), true
		}
	}
	uid, uerr := strconv.Atoi(get(MetaUID))
	gid, gerr := strconv.Atoi(get(MetaGID))
	if uerr == nil && gerr == nil && uid >= 0 && gid >= 0 && uint64(uid) < 1<<32-1 && uint64(gid) < 1<<32-1 {
		p.UID, p.GID, p.HasOwner = uid, gid, true
	}
	if t, err := time.Parse(time.RFC3339Nano, get(MetaMTime)); err == nil {
		p.MTime = t
	}
	if t, err := time.Parse(time.RFC3339Nano, get(MetaATime)); err == nil {
		p.ATime = t
	}
	p.SymlinkDest = get(MetaSymlink)
	if encoded := get(MetaSymlinkBase64); encoded != "" {
		p.SymlinkDest = ""
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			p.SymlinkDest = string(decoded)
		}
	}
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

// IsSymlink reports whether the metadata describes a symbolic link.
func (p PosixMeta) IsSymlink() bool { return p.SymlinkDest != "" }
