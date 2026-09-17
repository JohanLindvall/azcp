package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/JohanLindvall/azcp/internal/codec"
)

// A blob written by a web pipeline is often stored already compressed, with
// Content-Encoding saying so, because that is how it will be served. Downloaded
// as-is it is a file of gibberish with a plausible name. --decompress expands
// it on arrival and drops the extension that said it was compressed. It is the
// mirror of --compress, and the two share codec so they cannot drift apart.

// decompressible reports whether a Content-Encoding is one that can be
// expanded here.
func decompressible(encoding string) bool {
	_, ok := codec.ByEncoding(encoding)
	return ok
}

// decompressFile expands the file in place and returns its final path, which
// loses the extension announcing the compression if it had one.
func decompressFile(path, encoding string) (string, error) {
	return decompressTo(context.Background(), path, encoding, decompressedName(path))
}

func decompressedName(path string) string {
	name, _ := codec.StripExtension(path)
	return name
}

func decompressTo(ctx context.Context, path, encoding, final string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return path, err
	}
	info, err := in.Stat()
	if err != nil {
		in.Close()
		return path, err
	}
	r, err := codec.NewReader(in, encoding)
	if err != nil {
		in.Close()
		return path, fmt.Errorf("cannot decompress %s: %w", path, err)
	}

	// Written beside the destination and renamed over it, so an interrupted
	// expansion cannot leave a half-expanded file in place of the real one.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".azcp-decompress-*")
	if err != nil {
		r.Close()
		in.Close()
		return path, err
	}
	tmpName := tmp.Name()

	_, copyErr := io.Copy(tmp, &contextReader{ctx: ctx, Reader: r})
	r.Close()
	if copyErr == nil {
		copyErr = tmp.Chmod(info.Mode().Perm())
	}
	tmpErr := tmp.Close()

	// Everything is closed before anything is renamed or removed. Unix does
	// not care, but Windows refuses to move over or delete a file that is
	// still open, and the compressed original would be left sitting beside its
	// own expansion.
	in.Close()

	if copyErr != nil {
		os.Remove(tmpName)
		return path, fmt.Errorf("cannot decompress %s: %w", path, copyErr)
	}
	if tmpErr != nil {
		os.Remove(tmpName)
		return path, tmpErr
	}

	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return path, err
	}
	if final != path {
		// The compressed original is gone: it has become the expanded file
		// under a different name.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return final, fmt.Errorf("expanded %s but could not remove it: %w", path, err)
		}
	}
	return final, nil
}

// contextReader stops a copy when its context does.
type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
