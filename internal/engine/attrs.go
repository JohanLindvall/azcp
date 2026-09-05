package engine

import (
	"maps"
	"os"
	"time"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
)

// --preserve used to be silently ignored whenever the destination was a blob,
// because blob storage has no file mode and no owner. It has metadata, though,
// which is enough to carry them: these two functions are what let `azcp -a`
// put a filesystem tree into a container and get the same tree back out.

// uploadMetadata is what to store on a blob being written from a local file:
// the user's own --metadata, plus whatever --preserve asked to keep.
func (e *Engine) uploadMetadata(src *store.Node) map[string]string {
	if len(e.opt.Metadata) == 0 && !e.preservesToBlob() {
		return nil
	}
	m := make(map[string]string, len(e.opt.Metadata)+5)
	maps.Copy(m, e.opt.Metadata)
	if !e.preservesToBlob() || src.URL.IsRemote() {
		return m
	}

	info, err := os.Lstat(src.URL.Path)
	if err != nil {
		e.log.Warn("cannot read attributes to preserve",
			"path", src.URL.Display(), "error", err)
		return m
	}
	var p store.PosixMeta
	if e.opt.Preserve.Mode {
		p.Mode, p.HasMode = info.Mode(), true
	}
	if e.opt.Preserve.Ownership {
		if uid, gid, ok := local.OwnerOf(info); ok {
			p.UID, p.GID, p.HasOwner = uid, gid, true
		}
	}
	if e.opt.Preserve.Timestamps {
		p.MTime = info.ModTime()
		p.ATime = local.AccessTimeOf(info)
	}
	if src.IsSymlink() {
		p.SymlinkDest = src.LinkTarget
		// A link's own mode is meaningless; what matters is where it points.
		p.HasMode = false
	}
	maps.Copy(m, p.Encode())
	if len(m) == 0 {
		return nil
	}
	return m
}

// restoreAttrs applies attributes carried in a blob's metadata to the local
// file just written from it. Anything absent is left as it is rather than
// invented.
func (e *Engine) restoreAttrs(t *task) {
	if !e.preservesToBlob() || !t.src.URL.IsRemote() || t.dst.IsRemote() {
		return
	}
	p := store.DecodePosixMeta(t.src.Metadata)
	path := t.dst.Path

	// Ownership first: a successful chown clears the setuid and setgid bits,
	// so setting the mode afterwards is the only way they survive.
	if e.opt.Preserve.Ownership && p.HasOwner {
		if err := local.Lchown(path, p.UID, p.GID); err != nil {
			// Restoring ownership needs privilege the caller usually lacks;
			// worth saying, not worth failing the copy over.
			e.log.Warn("cannot restore ownership",
				"path", t.dst.Display(), "uid", p.UID, "gid", p.GID, "error", err)
		}
	}
	if e.opt.Preserve.Mode && p.HasMode {
		if err := os.Chmod(path, p.Mode); err != nil {
			e.log.Warn("cannot restore mode", "path", t.dst.Display(), "error", err)
		}
	}
	if e.opt.Preserve.Timestamps {
		mtime, atime := p.MTime, p.ATime
		if mtime.IsZero() {
			// Nothing preserved on the blob — most likely it was put there by
			// something other than azcp — so the service's own record of when
			// it was last written is the best available.
			mtime = t.src.ModTime
		}
		if atime.IsZero() {
			atime = mtime
		}
		if !mtime.IsZero() {
			if err := os.Chtimes(path, atime, mtime); err != nil {
				e.log.Warn("cannot restore timestamps", "path", t.dst.Display(), "error", err)
			}
		}
	}
}

// preservesToBlob reports whether anything worth storing in metadata was asked
// for.
func (e *Engine) preservesToBlob() bool {
	p := e.opt.Preserve
	return p.Mode || p.Ownership || p.Timestamps
}

// blobMTime is the timestamp to use for a blob when deciding whether it is
// newer than something: the preserved one if it is there, otherwise the last
// time the service saw it written.
func blobMTime(n *store.Node) time.Time {
	if n.URL.IsRemote() && len(n.Metadata) > 0 {
		if p := store.DecodePosixMeta(n.Metadata); !p.MTime.IsZero() {
			return p.MTime
		}
	}
	return n.ModTime
}
