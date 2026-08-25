package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
	kzlib "github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
)

// payload is compressible enough to exercise a real decode, rather than a
// stream the encoder gives up on.
func payload() []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < 512<<10; i++ {
		b.WriteString("the quick brown fox jumps over the lazy dog ")
		if i%17 == 0 {
			b.WriteString("\n-- a line that breaks the pattern up --\n")
		}
	}
	return b.Bytes()
}

func compress(t *testing.T, encoding string, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var w io.WriteCloser
	var err error
	switch encoding {
	case "gzip":
		w = kgzip.NewWriter(&out)
	case "deflate":
		w = kzlib.NewWriter(&out)
	case "zstd":
		w, err = zstd.NewWriter(&out)
	default:
		t.Fatalf("unknown encoding %q", encoding)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDecompressFile(t *testing.T) {
	data := payload()
	for _, tc := range []struct{ encoding, ext, wantName string }{
		{"gzip", ".gz", "file.txt"},
		{"gzip", ".gzip", "file.txt"},
		{"deflate", ".zz", "file.txt"},
		{"zstd", ".zst", "file.txt"},
		{"zstd", ".zstd", "file.txt"},
		{"gzip", "", "file.txt"}, // no extension to drop
	} {
		t.Run(tc.encoding+tc.ext, func(t *testing.T) {
			d := t.TempDir()
			path := filepath.Join(d, "file.txt"+tc.ext)
			if err := os.WriteFile(path, compress(t, tc.encoding, data), 0o644); err != nil {
				t.Fatal(err)
			}
			final, err := decompressFile(path, tc.encoding)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(final) != tc.wantName {
				t.Errorf("final name = %q, want %q", filepath.Base(final), tc.wantName)
			}
			got, err := os.ReadFile(final)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Errorf("content differs: %d bytes vs %d", len(got), len(data))
			}
			if tc.ext != "" && exists(path) {
				t.Error("the compressed original was left behind")
			}
		})
	}
}

func TestDecompressible(t *testing.T) {
	for _, yes := range []string{"gzip", "GZIP", "x-gzip", "deflate", "zstd", " zstd "} {
		if !decompressible(yes) {
			t.Errorf("decompressible(%q) = false", yes)
		}
	}
	for _, no := range []string{"", "identity", "br", "compress"} {
		if decompressible(no) {
			t.Errorf("decompressible(%q) = true", no)
		}
	}
}

// Rubbish must be reported, and must not leave a half-expanded file where the
// real one was.
func TestDecompressRejectsRubbish(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "bad.gz")
	if err := os.WriteFile(path, []byte("not compressed at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := decompressFile(path, "gzip"); err == nil {
		t.Fatal("rubbish was accepted as gzip")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "not compressed at all" {
		t.Errorf("the original was disturbed: %q, %v", got, err)
	}
	if entries, _ := os.ReadDir(d); len(entries) != 1 {
		t.Errorf("temporary files left behind: %d entries", len(entries))
	}
}

func BenchmarkDecodeGzip(b *testing.B) {
	data := compressForBench("gzip")
	b.SetBytes(int64(len(rawForBench())))
	b.ResetTimer()
	for b.Loop() {
		r, err := decoder(bytes.NewReader(data), "gzip")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatal(err)
		}
		r.Close()
	}
}

var benchRaw []byte

func rawForBench() []byte {
	if benchRaw == nil {
		var s strings.Builder
		for s.Len() < 8<<20 {
			s.WriteString("the quick brown fox jumps over the lazy dog 0123456789\n")
		}
		benchRaw = []byte(s.String())
	}
	return benchRaw
}

func compressForBench(encoding string) []byte {
	var out bytes.Buffer
	w := kgzip.NewWriter(&out)
	w.Write(rawForBench())
	w.Close()
	return out.Bytes()
}
