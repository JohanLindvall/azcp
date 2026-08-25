package azure

import (
	"crypto/md5"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/azcp/internal/retryx"
)

func writeTemp(t *testing.T, content string) (path string, sum []byte) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "blob.bin")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h := md5.Sum([]byte(content))
	return path, h[:]
}

func TestVerifyDownload(t *testing.T) {
	s := &Store{log: slog.New(slog.DiscardHandler)}
	path, good := writeTemp(t, "the bytes that were meant to arrive")
	bad := make([]byte, len(good))
	copy(bad, good)
	bad[0] ^= 0xff

	t.Run("matching checksum passes in every mode", func(t *testing.T) {
		for _, mode := range []MD5Check{MD5Off, MD5Warn, MD5Fail, MD5Require} {
			if err := s.verifyDownload(path, good, mode, "blob"); err != nil {
				t.Errorf("mode %v: %v", mode, err)
			}
		}
	})

	t.Run("mismatch fails, and is worth retrying", func(t *testing.T) {
		err := s.verifyDownload(path, bad, MD5Fail, "blob")
		if err == nil {
			t.Fatal("a corrupted download was accepted")
		}
		// The bytes arrived wrong; they may well arrive right next time, so
		// this must reach the retry loop rather than ending the transfer.
		if !retryx.IsTransient(err) {
			t.Error("a checksum mismatch was not treated as retryable")
		}
		if !errors.Is(err, retryx.ErrRetryable) {
			t.Error("mismatch did not carry the retryable marker")
		}
	})

	t.Run("mismatch is tolerated in warn and off", func(t *testing.T) {
		for _, mode := range []MD5Check{MD5Off, MD5Warn} {
			if err := s.verifyDownload(path, bad, mode, "blob"); err != nil {
				t.Errorf("mode %v rejected: %v", mode, err)
			}
		}
	})

	t.Run("a blob with no checksum", func(t *testing.T) {
		for _, mode := range []MD5Check{MD5Off, MD5Warn, MD5Fail} {
			if err := s.verifyDownload(path, nil, mode, "blob"); err != nil {
				t.Errorf("mode %v should accept a blob with no checksum: %v", mode, err)
			}
		}
		if err := s.verifyDownload(path, nil, MD5Require, "blob"); err == nil {
			t.Error("require should refuse a blob with no checksum")
		}
	})

	t.Run("an unreadable file is an error, not a pass", func(t *testing.T) {
		if err := s.verifyDownload(filepath.Join(t.TempDir(), "gone"), good, MD5Fail, "blob"); err == nil {
			t.Error("a missing destination was reported as verified")
		}
	})
}

func TestParseMD5Check(t *testing.T) {
	cases := map[string]MD5Check{
		"": MD5Fail, "fail": MD5Fail, "off": MD5Off, "none": MD5Off,
		"warn": MD5Warn, "log": MD5Warn, "require": MD5Require, "REQUIRE": MD5Require,
	}
	for in, want := range cases {
		got, err := ParseMD5Check(in)
		if err != nil || got != want {
			t.Errorf("ParseMD5Check(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseMD5Check("maybe"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
