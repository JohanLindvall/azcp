package azure

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/retryx"
)

// Blob storage will carry a Content-MD5 for a blob and hand it back on
// download, which is the only end-to-end check available: it covers everything
// between reading the source file and writing the destination one, including
// the parts of the path that TLS and the per-request checksums do not.
//
// It is not free. The hash has to be computed over the whole file, and neither
// direction can do it while transferring, because blocks and ranges are moved
// out of order. So both are opt-in, and both say plainly when they cannot do
// what was asked.

// MD5Check selects what to do about a blob's recorded checksum on download.
type MD5Check int

const (
	// MD5Off does not check.
	MD5Off MD5Check = iota
	// MD5Warn logs a mismatch and carries on.
	MD5Warn
	// MD5Fail treats a mismatch as a failed transfer. A blob with no recorded
	// checksum is accepted, since most blobs have none.
	MD5Fail
	// MD5Require additionally fails when the blob has no checksum at all.
	MD5Require
)

// ParseMD5Check maps a --check-md5 value.
func ParseMD5Check(s string) (MD5Check, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "no", "none":
		return MD5Off, nil
	case "warn", "log":
		return MD5Warn, nil
	case "", "fail":
		return MD5Fail, nil
	case "require":
		return MD5Require, nil
	}
	return 0, fmt.Errorf("unknown --check-md5 value %q (want off, warn, fail or require)", s)
}

// fileMD5 hashes a whole file.
func fileMD5(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// verifyDownload compares what landed on disk with the checksum the service
// reported for the blob.
func (s *Store) verifyDownload(path string, blobMD5 []byte, mode MD5Check, display string) error {
	if mode == MD5Off {
		return nil
	}
	if len(blobMD5) == 0 {
		if mode == MD5Require {
			return fmt.Errorf("%s has no recorded MD5 to check against "+
				"(use --check-md5=fail to accept blobs without one)", display)
		}
		s.log.Debug("blob has no recorded MD5, nothing to check against", "blob", display)
		return nil
	}
	got, err := fileMD5(path)
	if err != nil {
		return fmt.Errorf("cannot re-read %s to check it: %w", path, err)
	}
	if bytes.Equal(got, blobMD5) {
		s.log.Debug("checksum verified", "blob", display)
		return nil
	}
	err = fmt.Errorf("checksum mismatch for %s: the blob records %s but what arrived is %s: %w",
		display, base64.StdEncoding.EncodeToString(blobMD5),
		base64.StdEncoding.EncodeToString(got), retryx.ErrRetryable)
	if mode == MD5Warn {
		s.log.Warn("checksum mismatch, keeping the file anyway",
			"blob", display, "detail", err)
		return nil
	}
	return err
}
