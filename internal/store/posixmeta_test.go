package store

import (
	"io/fs"
	"testing"
	"time"
)

func TestPosixMetaRoundTrip(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 34, 56, 789012345, time.UTC)
	in := PosixMeta{
		Mode: 0o750 | fs.ModeSetgid | fs.ModeSticky, HasMode: true,
		UID: 1000, GID: 2000, HasOwner: true,
		MTime: when, ATime: when.Add(-time.Hour),
	}
	got := DecodePosixMeta(in.Encode())

	if !got.HasMode || got.Mode != in.Mode {
		t.Errorf("mode = %v (%v), want %v", got.Mode, got.HasMode, in.Mode)
	}
	if !got.HasOwner || got.UID != 1000 || got.GID != 2000 {
		t.Errorf("owner = %d/%d (%v)", got.UID, got.GID, got.HasOwner)
	}
	if !got.MTime.Equal(when) {
		t.Errorf("mtime = %v, want %v", got.MTime, when)
	}
	if !got.ATime.Equal(when.Add(-time.Hour)) {
		t.Errorf("atime = %v", got.ATime)
	}
}

func TestPosixMetaStoresTraditionalBits(t *testing.T) {
	// The stored value should be what stat would show, not Go's internal
	// FileMode representation, so a human reading the metadata recognises it.
	m := PosixMeta{Mode: 0o644, HasMode: true}.Encode()
	if got := m[MetaMode]; got != "0644" {
		t.Errorf("mode encoded as %q, want %q", got, "0644")
	}
	m = PosixMeta{Mode: 0o755 | fs.ModeSetuid, HasMode: true}.Encode()
	if got := m[MetaMode]; got != "04755" {
		t.Errorf("setuid mode encoded as %q, want %q", got, "04755")
	}
}

func TestPosixMetaSymlink(t *testing.T) {
	m := PosixMeta{SymlinkDest: "../target.txt"}.Encode()
	got := DecodePosixMeta(m)
	if !got.IsSymlink() || got.SymlinkDest != "../target.txt" {
		t.Errorf("symlink = %q", got.SymlinkDest)
	}
	if DecodePosixMeta(nil).IsSymlink() {
		t.Error("absent metadata reported as a symlink")
	}
}

// Rubbish in the metadata must be ignored, not guessed at: restoring nothing
// beats restoring a wrong mode.
func TestPosixMetaIgnoresRubbish(t *testing.T) {
	got := DecodePosixMeta(map[string]string{
		MetaMode: "not-octal", MetaUID: "x", MetaGID: "y", MetaMTime: "yesterday",
	})
	if got.HasMode || got.HasOwner || !got.MTime.IsZero() {
		t.Errorf("unparseable metadata was accepted: %+v", got)
	}
}

// Azure is case-insensitive about metadata names.
func TestPosixMetaCaseInsensitive(t *testing.T) {
	got := DecodePosixMeta(map[string]string{"AZCP_MODE": "0640"})
	if !got.HasMode || got.Mode.Perm() != 0o640 {
		t.Errorf("uppercase key not read: %+v", got)
	}
}
