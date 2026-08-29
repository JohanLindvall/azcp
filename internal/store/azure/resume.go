package azure

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"

	"github.com/JohanLindvall/azcp/internal/store"
)

// Resuming an interrupted transfer needs to know what already arrived, and the
// two directions answer that very differently.
//
// Uploading, the service knows: blocks staged but never committed are held
// against the blob and can be listed. So resuming an upload needs no state on
// this machine at all — it survives a reboot, and a different machine can pick
// the transfer up. That is better than a job-plan file, not merely equal to it.
//
// Downloading, nothing but this process knows which ranges landed, because they
// arrive out of order and a half-written file is indistinguishable from a whole
// one with holes. That needs a record beside the file, which is written as each
// range completes and removed when the file is whole.

// resumeSuffix names the record. It sits beside the destination so that
// removing the destination removes the reason to keep it.
const resumeSuffix = ".azcp-part"

// resumeFile records which ranges of a download have landed.
type resumeFile struct {
	path string
	mu   sync.Mutex
	f    *os.File
	have map[int]bool
}

// IncompleteDownload reports whether a resume record sits beside path, meaning
// the file there is a download that stopped part-way.
//
// Nothing else can tell. Ranges arrive out of order, so a partly written file
// is already the size of the whole blob and carries a timestamp from when it
// was last touched: to -n and -u it is indistinguishable from a finished copy,
// and skipping it would leave it that way for good.
func IncompleteDownload(path string) bool {
	_, err := os.Stat(path + resumeSuffix)
	return err == nil
}

// removeResumeRecord discards a record beside path. A download that ran without
// --resume has just written the whole file, so any record left by an earlier
// attempt describes something that no longer exists.
func removeResumeRecord(path string) error {
	err := os.Remove(path + resumeSuffix)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// openResumeFile opens or starts a record for this blob. A record describing a
// different blob — a different etag, size or block size — is discarded, because
// continuing into it would splice two files together.
func openResumeFile(dst string, src *store.Node, blockSize int64) (*resumeFile, error) {
	path := dst + resumeSuffix
	header := fmt.Sprintf("azcp-resume 1 %s %d %d",
		strings.Trim(src.ETag, `"`), src.Size, blockSize)

	r := &resumeFile{path: path, have: map[int]bool{}}
	if existing, err := os.Open(path); err == nil {
		matched := r.read(existing, header)
		existing.Close()
		if !matched {
			r.have = map[int]bool{}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
		}
	}

	fresh := len(r.have) == 0
	// A fresh record truncates at open rather than truncating the handle
	// afterwards: Windows refuses Truncate on a file opened for appending,
	// which quietly left --resume downloads there with no record at all.
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fresh {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	r.f = f
	if fresh {
		if _, err := fmt.Fprintln(f, header); err != nil {
			f.Close()
			return nil, err
		}
	}
	return r, nil
}

// read loads a record, reporting whether it describes the same blob.
func (r *resumeFile) read(f *os.File, header string) bool {
	sc := bufio.NewScanner(f)
	if !sc.Scan() || sc.Text() != header {
		return false
	}
	for sc.Scan() {
		if i, err := strconv.Atoi(strings.TrimSpace(sc.Text())); err == nil {
			r.have[i] = true
		}
	}
	return sc.Err() == nil
}

func (r *resumeFile) has(i int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.have[i]
}

// mark records a completed range, on disk before it is believed.
func (r *resumeFile) mark(i int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.have[i] {
		return nil
	}
	if _, err := fmt.Fprintln(r.f, i); err != nil {
		return err
	}
	if err := r.f.Sync(); err != nil {
		return err
	}
	r.have[i] = true
	return nil
}

// bytesDone is how much of the blob an earlier run already fetched.
func (r *resumeFile) bytesDone(blockSize, total int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for i := range r.have {
		offset := int64(i) * blockSize
		n += min(blockSize, total-offset)
	}
	return n
}

// done removes the record: the file is whole and there is nothing to resume.
func (r *resumeFile) done() {
	r.close()
	_ = os.Remove(r.path)
}

func (r *resumeFile) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}

// stagedBlocks asks the service which blocks of this blob were staged by an
// earlier attempt and never committed. This is what makes resuming an upload
// need nothing on this machine.
func (s *Store) stagedBlocks(ctx context.Context, bb *blockblob.Client) (map[string]bool, error) {
	resp, err := bb.GetBlockList(ctx, blockblob.BlockListTypeUncommitted, nil)
	if err != nil {
		// No staged blocks, or a blob that does not exist yet: nothing to
		// resume, which is not an error.
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	staged := make(map[string]bool, len(resp.UncommittedBlocks))
	for _, b := range resp.UncommittedBlocks {
		if b.Name != nil {
			staged[*b.Name] = true
		}
	}
	return staged, nil
}

// blockID renders a block index as the fixed-width, base64 identifier the
// service requires. Every block in one blob must encode to the same length.
func blockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("azcp-blk-%08d", i)))
}
