package engine

import (
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// destIndex answers "is this already there?" from one directory listing rather
// than a stat per file.
//
// It exists for the rerun. A mirroring copy asked to fetch only what is
// missing — `-n --resume`, the shape an interrupted download is picked up with
// — puts that question to every name in the source and gets "yes" for nearly
// all of them. Answered by stat, each one costs two system calls, the file and
// then the resume record beside it, on the single goroutine that also has to
// keep the workers fed. Answered from the directory's own entries it costs one
// getdents batch for the whole directory and nothing at all per file.
//
// Only existence is cached, because existence is the whole of what -n and
// --update=none decide on. Anything that weighs the two sides — -u against a
// timestamp, -i, --backup — stats as it always did, so no decision is ever made
// on a fact this cannot supply.
type destIndex struct {
	dirs map[string]*dirEntries
	// lru is the directories in hand, least recently used first. The walk
	// arrives in lexical order, so what is in play at any moment is the
	// directory being filled and the ancestors a deeper name has interrupted
	// it with; the list stays short on its own and is bounded in case it does
	// not.
	lru []string
	// names counts what is cached across every directory, which is what
	// bounds the memory this costs.
	names int
	// reads counts the directories actually listed, which is the number the
	// tests hold this to: one per directory, not one per file in it.
	reads int
}

const (
	// maxIndexDirs is how many directories are kept at once.
	maxIndexDirs = 16
	// maxIndexNames bounds the names kept across them all. A directory larger
	// than this is still indexed — it is the one being asked about — but
	// nothing is kept beside it.
	maxIndexNames = 1 << 20
)

// dirEntries is one directory's names.
type dirEntries struct {
	names map[string]struct{}
	// parts holds the names an unfinished download left a record for, which is
	// the one thing that outranks -n. It is nil in the ordinary case where no
	// download was interrupted, so asking costs nothing.
	parts map[string]struct{}
}

func newDestIndex() *destIndex {
	return &destIndex{dirs: map[string]*dirEntries{}}
}

// lookup reports whether something is already at u and whether a resume record
// sits beside it. ok is false when the index cannot answer and the caller must
// stat.
func (d *destIndex) lookup(u *uri.URL) (exists, partial, ok bool) {
	if d == nil || u.IsRemote() {
		return false, false, false
	}
	dir, name := splitDir(u.Path)
	entries := d.read(dir)
	if entries == nil {
		return false, false, false
	}
	_, exists = entries.names[name]
	_, partial = entries.parts[name]
	return exists, partial, true
}

// planned records a name this run is about to write, so that a second source
// argument covering the same tree sees it the way a stat would have. Without
// it two sources holding the same relative path would both be queued and both
// written, which -n exists to prevent.
func (d *destIndex) planned(u *uri.URL) {
	if d == nil || u.IsRemote() {
		return
	}
	dir, name := splitDir(u.Path)
	// Only a directory already in hand is updated: reading one in order to
	// record a file that is not written yet would cost the very listing this
	// is here to avoid.
	if entries, ok := d.dirs[dir]; ok {
		if _, seen := entries.names[name]; !seen {
			entries.names[name] = struct{}{}
			d.names++
		}
	}
}

// read returns the directory's entries, listing it on first use. A directory
// that is not there yet reads as empty, which is the true answer and saves the
// same failing stat for every file in it — the shape a --dry-run rerun has,
// since it creates no directories.
func (d *destIndex) read(dir string) *dirEntries {
	if entries, ok := d.dirs[dir]; ok {
		d.touch(dir)
		return entries
	}
	entries := &dirEntries{names: map[string]struct{}{}}
	d.reads++
	f, err := os.Open(dir)
	switch {
	case err == nil:
		names, rerr := f.Readdirnames(-1)
		f.Close()
		if rerr != nil {
			// A listing that stopped part-way would report files that are
			// there as missing, which -n would answer by overwriting them.
			return nil
		}
		for _, n := range names {
			if base, cut := strings.CutSuffix(n, azure.ResumeSuffix); cut {
				if entries.parts == nil {
					entries.parts = map[string]struct{}{}
				}
				entries.parts[base] = struct{}{}
				continue
			}
			entries.names[n] = struct{}{}
		}
	case os.IsNotExist(err):
	default:
		return nil
	}

	d.dirs[dir] = entries
	d.lru = append(d.lru, dir)
	d.names += len(entries.names) + len(entries.parts)
	d.evict()
	return entries
}

// touch moves a directory to the end of the eviction order.
func (d *destIndex) touch(dir string) {
	if len(d.lru) == 0 || d.lru[len(d.lru)-1] == dir {
		return
	}
	for i, name := range d.lru {
		if name == dir {
			d.lru = append(append(d.lru[:i], d.lru[i+1:]...), dir)
			return
		}
	}
}

// evict drops the least recently used directories until both bounds hold. The
// one just read is never dropped, however large it is: it is the directory
// being asked about, and losing it would mean listing it again immediately.
func (d *destIndex) evict() {
	for len(d.lru) > 1 && (len(d.lru) > maxIndexDirs || d.names > maxIndexNames) {
		oldest := d.lru[0]
		if entries, ok := d.dirs[oldest]; ok {
			d.names -= len(entries.names) + len(entries.parts)
			delete(d.dirs, oldest)
		}
		d.lru = d.lru[1:]
	}
}

// splitDir divides a local path into the directory to list and the name to
// look for in it. Paths reach here with "/" separators whatever the platform,
// since that is how uri.Parse stores them.
func splitDir(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	switch {
	case i < 0:
		return ".", p
	case i == 0:
		return "/", p[1:]
	default:
		return p[:i], p[i+1:]
	}
}
