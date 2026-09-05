package azure

import (
	"bytes"
	"context"
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

// The hash runs beside the upload and is collected only when the commit needs
// it; nothing asked for is nothing waited on.
func TestChecksumRunsInTheBackground(t *testing.T) {
	path, want := writeTemp(t, "some bytes worth hashing")
	got, err := startChecksum(context.Background(), path).wait()
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("checksum = %x, %v; want %x", got, err, want)
	}
	var none *checksum
	if sum, err := none.wait(); sum != nil || err != nil {
		t.Errorf("an unrequested checksum yielded %x, %v", sum, err)
	}
	if _, err := startChecksum(context.Background(), filepath.Join(t.TempDir(), "gone")).wait(); err == nil {
		t.Error("a missing file hashed to something")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := startChecksum(ctx, path).wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled hash returned %v", err)
	}
}
