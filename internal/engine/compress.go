package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/JohanLindvall/azcp/internal/codec"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// --compress is the mirror of --decompress. A file goes up compressed, under
// its name plus the format's extension and with Content-Encoding saying which
// — how a web pipeline expects a blob it will serve compressed to look, and
// exactly what --decompress turns back into the file it started as. A copy
// that never leaves the filesystem gets the same treatment without the header.

// compresses reports whether --compress applies to src: a local regular file
// that is not already in a compressed form — a .gz is copied as it is rather
// than wrapped twice — or a symbolic link the walk is about to resolve into
// one. --attributes-only leaves names alone, as it does for --decompress.
func (e *Engine) compresses(src *store.Node) bool {
	if !e.opt.Compress.On() || e.opt.AttributesOnly || src.URL.IsRemote() ||
		codec.HasExtension(src.Name()) {
		return false
	}
	if src.IsRegular() || src.IsPipe() {
		return true
	}
	return src.IsSymlink() && !src.Mode.IsDir() && e.derefAt(false)
}

// compressedDestination appends the format's extension, unless the destination
// was named with it already.
func (e *Engine) compressedDestination(dst *uri.URL) *uri.URL {
	ext := e.opt.Compress.Format.Extension()
	if strings.HasSuffix(dst.PathPart(), ext) {
		return dst
	}
	return dst.WithPathPart(dst.PathPart() + ext)
}

// compressInto writes the compressed form of the file at path to w. Progress
// is measured on the bytes consumed from the source, which is what the task
// was sized by; how long the result will be is not known until it ends.
func (e *Engine) compressInto(ctx context.Context, w io.Writer, path string, pt *progress.Task) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = e.compressFrom(ctx, w, f, pt)
	return err
}

// compressFrom is encode with the source measured, and reports how much of it
// there turned out to be.
func (e *Engine) compressFrom(ctx context.Context, w io.Writer, source io.Reader, pt *progress.Task) (int64, error) {
	src := &meter{Reader: &contextReader{ctx: ctx, Reader: source}, report: pt.Set}
	err := e.encode(w, src)
	if err == nil {
		pt.SetSize(src.n)
	}
	return src.n, err
}

// encode writes what r delivers to w in the --compress format.
func (e *Engine) encode(w io.Writer, r io.Reader) error {
	enc, err := e.opt.Compress.NewWriter(w)
	if err != nil {
		return err
	}
	_, err = io.Copy(enc, r)
	if cerr := enc.Close(); err == nil {
		err = cerr
	}
	return err
}

// meter reports the running total read through it.
type meter struct {
	io.Reader
	n      int64
	report func(int64)
}

func (m *meter) Read(p []byte) (int, error) {
	n, err := m.Reader.Read(p)
	if n > 0 {
		m.n += int64(n)
		m.report(m.n)
	}
	return n, err
}

// encoded returns the file compressed on the fly, for an upload that reads a
// stream. Each call starts afresh, so an attempt that fails part-way can be
// repeated from the beginning.
func (e *Engine) encoded(ctx context.Context, path string, pt *progress.Task) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(e.compressInto(ctx, pw, path, pt)) }()
	return pr
}

// compressLocal is the filesystem-to-filesystem copy under --compress: the
// destination is written through the encoder rather than cloned or copied.
func (e *Engine) compressLocal(ctx context.Context, t *task, pt *progress.Task) error {
	// Open the source first, and compare the open files before truncating.
	// A read failure must not destroy the old destination, and aliases can
	// change between planning and the worker reaching this file.
	source, err := os.Open(t.src.URL.Path)
	if err != nil {
		return destError(t, err)
	}
	defer source.Close()
	si, err := source.Stat()
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE
	if e.opt.NoClobber {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := e.openDest(t, flags, e.opt.CreationMode(t.src.Mode))
	if err != nil {
		return err
	}
	defer f.Close()
	di, err := f.Stat()
	if err != nil {
		return err
	}
	if os.SameFile(si, di) {
		return destError(t, local.ErrSameFile)
	}
	if di.Mode().IsRegular() {
		if err := f.Truncate(0); err != nil {
			return err
		}
	}
	n, err := e.compressFrom(ctx, f, source, pt)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if phrased, ok := ioError(t, err); ok {
			return phrased
		}
		return fmt.Errorf("cannot write %s: %w", quote(t.dst.Display()), err)
	}
	if t.src.IsPipe() {
		t.src.Size = n
	}
	return nil
}
