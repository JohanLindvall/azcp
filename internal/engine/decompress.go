package engine

import (
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"os"
	"strings"
)

// A blob written by a web pipeline is often stored already compressed, with
// Content-Encoding saying so, because that is how it will be served. Downloaded
// as-is it is a file of gibberish with a plausible name. --decompress expands
// it on arrival and drops the extension that said it was compressed.

// decompressed reports the encodings this understands.
func decompressible(encoding string) bool {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip", "deflate":
		return true
	}
	return false
}

// decompressFile expands the file in place and returns its final path, which
// loses a trailing .gz, .gzip or .zz if it had one.
func decompressFile(path, encoding string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return path, err
	}
	defer in.Close()

	r, err := decoder(in, encoding)
	if err != nil {
		return path, fmt.Errorf("cannot decompress %s: %w", path, err)
	}
	defer r.Close()

	// Written beside the destination and renamed over it, so an interrupted
	// expansion cannot leave a half-expanded file in place of the real one.
	tmp, err := os.CreateTemp(dirOf(path), ".azcp-decompress-*")
	if err != nil {
		return path, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return path, fmt.Errorf("cannot decompress %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return path, err
	}

	final := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(path,
		".gz"), ".gzip"), ".zz")
	if err := os.Rename(tmpName, final); err != nil {
		return path, err
	}
	if final != path {
		// The compressed original is gone: it has become the expanded file
		// under a different name.
		_ = os.Remove(path)
	}
	return final, nil
}

func decoder(r io.Reader, encoding string) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		return gzip.NewReader(r)
	case "deflate":
		// "deflate" is ambiguous in the wild: the specification says zlib, and
		// a good deal of software means raw. Try the correct one, fall back to
		// what people actually send.
		if zr, err := zlib.NewReader(r); err == nil {
			return zr, nil
		}
		if s, ok := r.(io.Seeker); ok {
			if _, err := s.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
		}
		return flate.NewReader(r), nil
	}
	return nil, fmt.Errorf("unknown content encoding %q", encoding)
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[:i]
	}
	return "."
}
