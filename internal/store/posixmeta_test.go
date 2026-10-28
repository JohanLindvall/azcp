// SPDX-License-Identifier: MIT

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

func TestPosixMetadataRejectsValuesThatWouldTruncate(t *testing.T) {
	for _, value := range []string{"-1", "4294967295", "4294967296"} {
		if got := DecodePosixMeta(map[string]string{MetaUID: value, MetaGID: "1"}); got.HasOwner {
			t.Fatalf("invalid owner %s accepted", value)
		}
	}
	if DecodePosixMeta(map[string]string{MetaMode: "65536"}).HasMode {
		t.Fatal("invalid mode accepted")
	}

}

func TestPosixMetaStoresTraditionalBits(t *testing.T) {
	// AzCopy stores the full Unix st_mode as a decimal integer.
	m := PosixMeta{Mode: 0o644, HasMode: true}.Encode()
	if got := m[MetaMode]; got != "33188" {
		t.Errorf("mode encoded as %q, want %q", got, "33188")
	}
	m = PosixMeta{Mode: 0o755 | fs.ModeSetuid, HasMode: true}.Encode()
	if got := m[MetaMode]; got != "35309" {
		t.Errorf("setuid mode encoded as %q, want %q", got, "35309")
	}
}

func TestPosixMetaSymlink(t *testing.T) {
	m := PosixMeta{Symlink: true}.Encode()
	got := DecodePosixMeta(m)
	if !got.IsSymlink() {
		t.Errorf("symlink marker = %v", m)
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
	got := DecodePosixMeta(map[string]string{"PERMISSIONS": "33184"})
	if !got.HasMode || got.Mode.Perm() != 0o640 {
		t.Errorf("uppercase key not read: %+v", got)
	}
}

func TestAzCopyPOSIXFixtures(t *testing.T) {
	for _, metadata := range []map[string]string{
		{"permissions": "33256", "posix_owner": "1000", "posix_group": "2000", "modtime": "1709296496789012345", "posix_atime": "1709292896789012345", "linux_statx_mask": "2047"},
		{"permissions": "0750", "owner": "1000", "group": "2000", "modtime": "2024-03-01 12:34:56 +0000", "posix_atime": "1709292896789012345"},
	} {
		p := DecodePosixMeta(metadata)
		if !p.HasMode || p.Mode.Perm() != 0750 || !p.HasOwner || p.UID != 1000 || p.GID != 2000 || p.MTime.Unix() != 1709296496 || p.ATime.UnixNano() != 1709292896789012345 {
			t.Fatalf("AzCopy metadata decoded incorrectly: %+v", p)
		}
	}
	p := DecodePosixMeta(map[string]string{"permissions": "33188", "posix_owner": "1000", "posix_group": "2000", "modtime": "123456789", "linux_statx_mask": "0"})
	if p.HasMode || p.HasOwner || !p.MTime.IsZero() {
		t.Fatalf("statx mask ignored: %+v", p)
	}
	for _, value := range []string{"false", "../target", "1"} {
		if DecodePosixMeta(map[string]string{"is_symlink": value}).IsSymlink() {
			t.Fatalf("invalid symlink marker %q", value)
		}
	}
}

func TestRetiredMetadataIsNotInterpreted(t *testing.T) {
	p := DecodePosixMeta(map[string]string{
		"azcp_mode": "0755", "azcp_uid": "1000", "azcp_gid": "1000",
		"azcp_mtime": "2024-01-01T00:00:00Z", "azcp_atime": "2024-01-01T00:00:00Z",
		"azcp_symlink": "../target", "azcp_symlink_base64": "Li4vdGFyZ2V0",
	})
	if p.HasMode || p.HasOwner || !p.MTime.IsZero() || !p.ATime.IsZero() || p.IsSymlink() {
		t.Fatalf("legacy metadata interpreted: %+v", p)
	}
}

func TestPartialPOSIXPreservationAdvertisesOnlyKnownFields(t *testing.T) {
	m := (PosixMeta{MTime: time.Unix(123, 456)}).Encode()
	if m["linux_statx_mask"] != "64" || m["modtime"] != "123000000456" || len(m) != 2 {
		t.Fatalf("partial preservation: %v", m)
	}
}
