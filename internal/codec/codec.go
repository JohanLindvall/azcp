// Package codec names the compressed forms a copy can write or expand — gzip,
// deflate and zstd — and knows, for each, its Content-Encoding, its file
// extension and how to open a stream in it. --compress and --decompress share
// it so that they are exact mirrors: what one stores as report.csv.gz with
// Content-Encoding: gzip, the other reads back as report.csv.
package codec

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	// klauspost's implementations are drop-in replacements for the standard
	// library's and measurably quicker in both directions, which matters
	// because a file of any size passes through in one stream. They also
	// bring zstd, which the standard library has no answer for at all.
	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
)

// Format is one compressed form.
type Format int

const (
	// None is no compression: the zero value, so an unset option means off.
	None Format = iota
	// Gzip is RFC 1952, the form the web serves and every tool reads.
	Gzip
	// Deflate is zlib-wrapped deflate, RFC 1950, which HTTP calls "deflate".
	Deflate
	// Zstd is Zstandard, RFC 8878: faster than gzip at every ratio it reaches.
	Zstd
)

// String is the format's name as --compress and Content-Encoding spell it.
func (f Format) String() string {
	switch f {
	case Gzip:
		return "gzip"
	case Deflate:
		return "deflate"
	case Zstd:
		return "zstd"
	}
	return "none"
}

// Extension is the suffix a file in the format conventionally carries.
func (f Format) Extension() string {
	switch f {
	case Gzip:
		return ".gz"
	case Deflate:
		return ".zz"
	case Zstd:
		return ".zst"
	}
	return ""
}

// maxLevel is the strongest level the format accepts, on its own tool's scale.
func (f Format) maxLevel() int {
	if f == Zstd {
		return 19
	}
	return 9
}

// names are the spellings --compress accepts for each format.
var names = map[string]Format{
	"gzip": Gzip, "gz": Gzip,
	"deflate": Deflate, "zlib": Deflate,
	"zstd": Zstd, "zst": Zstd,
}

// extensions are the suffixes that announce a compressed form.
var extensions = map[string]Format{
	".gz": Gzip, ".gzip": Gzip,
	".zz":  Deflate,
	".zst": Zstd, ".zstd": Zstd,
}

// Spec is what --compress asks for: a format and, optionally, how hard to try.
type Spec struct {
	Format Format
	// Level is on the format's own scale — 1 to 9 for gzip and deflate, 1 to
	// 19 for zstd — or zero for the format's default.
	Level int
}

// On reports whether compression was asked for at all.
func (s Spec) On() bool { return s.Format != None }

func (s Spec) String() string {
	if s.Level == 0 {
		return s.Format.String()
	}
	return fmt.Sprintf("%s:%d", s.Format, s.Level)
}

// Parse reads a --compress value: a format, a format and level joined by a
// colon, a bare level meaning gzip at that level, or nothing at all for gzip
// at its default.
func Parse(spec string) (Spec, error) {
	name, level, hasLevel := strings.Cut(strings.ToLower(strings.TrimSpace(spec)), ":")
	if _, err := strconv.Atoi(name); err == nil && !hasLevel {
		// A bare number is a gzip level, as it is to gzip itself.
		name, level, hasLevel = "gzip", name, true
	}
	s := Spec{Format: Gzip}
	if name != "" {
		f, ok := names[name]
		if !ok {
			return Spec{}, fmt.Errorf("unknown compression %q (want gzip, deflate or zstd)", name)
		}
		s.Format = f
	}
	if hasLevel {
		n, err := strconv.Atoi(level)
		if err != nil || n < 1 || n > s.Format.maxLevel() {
			return Spec{}, fmt.Errorf("invalid %s level %q (want 1 to %d)",
				s.Format, level, s.Format.maxLevel())
		}
		s.Level = n
	}
	return s, nil
}

// NewWriter returns a writer that compresses what is written to it into w.
// Closing it finishes the stream; it does not close w.
func (s Spec) NewWriter(w io.Writer) (io.WriteCloser, error) {
	switch s.Format {
	case Gzip:
		return gzip.NewWriterLevel(w, s.levelOr(gzip.DefaultCompression))
	case Deflate:
		return zlib.NewWriterLevel(w, s.levelOr(zlib.DefaultCompression))
	case Zstd:
		level := zstd.SpeedDefault
		if s.Level != 0 {
			level = zstd.EncoderLevelFromZstd(s.Level)
		}
		// One goroutine per stream. The encoder would otherwise fan out over
		// every core for each file, and the run is already compressing --jobs
		// files at once.
		return zstd.NewWriter(w, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
	}
	return nil, errors.New("no compression format")
}

func (s Spec) levelOr(dflt int) int {
	if s.Level == 0 {
		return dflt
	}
	return s.Level
}

// ByEncoding maps a Content-Encoding to the format that produced it.
func ByEncoding(encoding string) (Format, bool) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		return Gzip, true
	case "deflate":
		return Deflate, true
	case "zstd":
		return Zstd, true
	}
	return None, false
}

// NewReader returns a reader that expands the stream r, whose Content-Encoding
// is encoding. "deflate" is ambiguous in the wild — the specification says
// zlib and a good deal of software means raw — so the correct one is tried
// first and, if r can be rewound, the other is fallen back to.
func NewReader(r io.Reader, encoding string) (io.ReadCloser, error) {
	f, ok := ByEncoding(encoding)
	if !ok {
		return nil, fmt.Errorf("unknown content encoding %q", encoding)
	}
	switch f {
	case Gzip:
		return gzip.NewReader(r)
	case Deflate:
		if zr, err := zlib.NewReader(r); err == nil {
			return zr, nil
		}
		if s, ok := r.(io.Seeker); ok {
			if _, err := s.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
		}
		return flate.NewReader(r), nil
	default:
		d, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return d.IOReadCloser(), nil
	}
}

// StripExtension drops the suffix announcing a compressed form — .gz, .gzip,
// .zz, .zst or .zstd, in any case — from a path, and reports whether there was
// one. A name that is nothing but the suffix is left alone.
func StripExtension(path string) (string, bool) {
	base := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(base))
	if _, ok := extensions[ext]; !ok || len(base) == len(ext) {
		return path, false
	}
	return path[:len(path)-len(ext)], true
}

// HasExtension reports whether name announces itself as already compressed.
func HasExtension(name string) bool {
	_, ok := StripExtension(name)
	return ok
}
