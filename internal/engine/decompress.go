package engine

import (
	"fmt"
	"io"
	"os"
	"strings"

	// klauspost's decoders are drop-in replacements for the standard
	// library's and measurably quicker, which matters here because a
	// download of any size is expanded in one pass. They also bring zstd,
	// which the standard library has no answer for at all.
	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
)

// A blob written by a web pipeline is often stored already compressed, with
// Content-Encoding saying so, because that is how it will be served. Downloaded
// as-is it is a file of gibberish with a plausible name. --decompress expands
// it on arrival and drops the extension that said it was compressed.

// decompressed reports the encodings this understands.
func decompressible(encoding string) bool {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip", "deflate", "zstd":
		return true
	}
	return false
}

// decompressFile expands the file in place and returns its final path, which
// loses a trailing .gz, .gzip, .zz, .zst or .zstd if it had one.
func decompressFile(path, encoding string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return path, err
	}
	r, err := decoder(in, encoding)
	if err != nil {
		in.Close()
		return path, fmt.Errorf("cannot decompress %s: %w", path, err)
	}

	// Written beside the destination and renamed over it, so an interrupted
	// expansion cannot leave a half-expanded file in place of the real one.
	tmp, err := os.CreateTemp(dirOf(path), ".azcp-decompress-*")
	if err != nil {
		r.Close()
		in.Close()
		return path, err
	}
	tmpName := tmp.Name()

	_, copyErr := io.Copy(tmp, r)
	r.Close()
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

	final := path
	for _, ext := range []string{".gz", ".gzip", ".zz", ".zst", ".zstd"} {
		if trimmed, ok := strings.CutSuffix(final, ext); ok {
			final = trimmed
			break
		}
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
	case "zstd":
		d, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	}
	return nil, fmt.Errorf("unknown content encoding %q", encoding)
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[:i]
	}
	return "."
}
