package azure

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// ResumeSuffix names the record. It sits beside the destination so that
// removing the destination removes the reason to keep it. It is exported
// because a caller already listing the destination directory can identify
// candidates without probing every destination. Its contents still establish
// ownership before it is trusted as a record.
const ResumeSuffix = ".azcp-part"

// resumeFile records which ranges of a download have landed.
type resumeFile struct {
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
	f, err := readResumeRecord(path + ResumeSuffix)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// RemoveResumeRecord discards a record beside path. A download that ran without
// --resume has just written the whole file, so any record left by an earlier
// attempt describes something that no longer exists.
func RemoveResumeRecord(path string) error {
	f, err := readResumeRecord(path + ResumeSuffix)
	if os.IsNotExist(err) || errors.Is(err, errNotResumeRecord) {
		return nil
	}
	if err != nil {
		return err
	}
	f.Close()
	err = os.Remove(path + ResumeSuffix)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

var errNotResumeRecord = errors.New("not an azcp resume record")

// A suffix alone is not proof of ownership: users can copy files bearing that
// suffix too. Never follow a sidecar symlink or read a pipe as a record.
func readResumeRecord(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, errNotResumeRecord)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, fmt.Errorf("%s changed while opening the resume record", path)
	}
	const magic = "azcp-resume "
	var prefix [len(magic)]byte
	n, err := io.ReadFull(f, prefix[:])
	owned := string(prefix[:n]) == magic
	if !owned || (err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF)) {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, errNotResumeRecord)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ResetResumeRecord invalidates ranges after a checksum or decoding failure.
// Replace the sidecar instead of truncating a path that may be a link.
func ResetResumeRecord(dst string) error {
	f, err := readResumeRecord(dst + ResumeSuffix)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if f != nil {
		f.Close()
	}
	r, err := rewriteResumeRecord(dst+ResumeSuffix, "azcp-resume invalid", nil)
	if r != nil {
		r.Close()
	}
	return err
}

// openResumeFile opens or starts a record for this blob. A record describing a
// different blob — a different etag, size or block size — is discarded, because
// continuing into it would splice two files together.
func openResumeFile(dst string, src *store.Node, blockSize int64) (*resumeFile, error) {
	path := dst + ResumeSuffix
	identity := sha256.Sum256([]byte(blobURL(src.URL)))
	header := fmt.Sprintf("azcp-resume 2 %x %s %d %d",
		identity, strings.Trim(src.ETag, `"`), src.Size, blockSize)

	r := &resumeFile{have: map[int]bool{}}
	if existing, err := readResumeRecord(path); err == nil {
		matched := r.read(existing, header, src.Size, blockSize)
		existing.Close()
		fi, err := os.Stat(dst)
		matched = matched && err == nil && fi.Size() == src.Size && src.ETag != ""
		if !matched {
			r.have = map[int]bool{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	f, err := rewriteResumeRecord(path, header, r.have)
	if err != nil {
		return nil, err
	}
	r.f = f
	return r, nil
}

// Replacing the record breaks hard-link aliases and publishes the header and
// prior ranges together. The record is closed before renaming for Windows.
func rewriteResumeRecord(path, header string, have map[int]bool) (*os.File, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".azcp-resume-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if header != "" {
		if _, err := fmt.Fprintln(tmp, header); err != nil {
			return nil, err
		}
	}
	indices := make([]int, 0, len(have))
	for i := range have {
		indices = append(indices, i)
	}
	slices.Sort(indices)
	for _, i := range indices {
		if _, err := fmt.Fprintln(tmp, i); err != nil {
			return nil, err
		}
	}
	info, err := tmp.Stat()
	if err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, fmt.Errorf("%s changed while opening the resume record", path)
	}
	return f, nil
}

// read loads a record, reporting whether it describes the same blob.
func (r *resumeFile) read(f *os.File, header string, size, blockSize int64) bool {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return false
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], info.Size()-1); err != nil || last[0] != '\n' {
		return false
	}
	sc := bufio.NewScanner(f)
	if !sc.Scan() || sc.Text() != header {
		return false
	}
	for sc.Scan() {
		i, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
		if err != nil || i < 0 || int64(i) >= (size+blockSize-1)/blockSize {
			return false
		}
		r.have[i] = true
	}
	return sc.Err() == nil
}

func (r *resumeFile) has(i int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.have[i]
}

// mark records a completed range, written before it is believed.
//
// Written, not fsynced. The record guards against the process ending — a
// Ctrl-C, a crash, a lost session — and for that the write suffices: the
// kernel has both the range and the line that vouches for it. An fsync here
// would promise nothing more after a power cut, since the data range it
// refers to is not flushed either, and it would serialise every parallel
// range of every download behind the disk's write latency.
func (r *resumeFile) mark(i int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.have[i] {
		return nil
	}
	if _, err := fmt.Fprintln(r.f, i); err != nil {
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
	return base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "azcp-blk-%08d", i))
}

// Content identities let a resumed upload trust only matching bytes, even if
// the source or block size changed. Index-only IDs silently splice old blocks
// into a new file. Keep the old decoded length for Azure's uniform-ID rule.
func contentBlockID(ctx context.Context, src io.ReaderAt, offset, size int64) (string, error) {
	section := io.NewSectionReader(src, offset, size)
	sum, err := hashReader(ctx, section, sha256.New())
	if err != nil {
		return "", err
	}
	if read, err := section.Seek(0, io.SeekCurrent); err != nil || read != size {
		return "", io.ErrUnexpectedEOF
	}
	return base64.StdEncoding.EncodeToString(sum[:17]), nil
}
