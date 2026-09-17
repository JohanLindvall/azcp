package codec

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	accepted := map[string]Spec{
		"":          {Format: Gzip},
		"gzip":      {Format: Gzip},
		"GZ":        {Format: Gzip},
		"gzip:9":    {Format: Gzip, Level: 9},
		"9":         {Format: Gzip, Level: 9},
		"deflate":   {Format: Deflate},
		"zlib:1":    {Format: Deflate, Level: 1},
		"zstd":      {Format: Zstd},
		" zst:19 ":  {Format: Zstd, Level: 19},
		"zstd:3":    {Format: Zstd, Level: 3},
		"DEFLATE:5": {Format: Deflate, Level: 5},
	}
	for in, want := range accepted {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	rejected := map[string]string{
		"lzma":    "unknown compression",
		"gzip:0":  "want 1 to 9",
		"gzip:10": "want 1 to 9",
		"zstd:20": "want 1 to 19",
		"zstd:x":  "invalid zstd level",
		"gzip:":   "invalid gzip level",
		"0":       "want 1 to 9",
	}
	for in, want := range rejected {
		_, err := Parse(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v; want an error mentioning %q", in, err, want)
		}
	}
}

func TestSpecString(t *testing.T) {
	if got := (Spec{Format: Zstd}).String(); got != "zstd" {
		t.Errorf("String() = %q", got)
	}
	if got := (Spec{Format: Gzip, Level: 6}).String(); got != "gzip:6" {
		t.Errorf("String() = %q", got)
	}
	if (Spec{}).On() || !(Spec{Format: Deflate}).On() {
		t.Error("On() disagrees with the zero value")
	}
}

// What NewWriter produces, NewReader reads back under the format's own
// Content-Encoding, at every level the option accepts.
func TestRoundTrip(t *testing.T) {
	payload := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog\n", 4000))
	for _, f := range []Format{Gzip, Deflate, Zstd} {
		for _, level := range []int{0, 1, f.maxLevel()} {
			spec := Spec{Format: f, Level: level}
			var buf bytes.Buffer
			w, err := spec.NewWriter(&buf)
			if err != nil {
				t.Fatalf("%v: %v", spec, err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatalf("%v: write: %v", spec, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("%v: close: %v", spec, err)
			}
			if buf.Len() >= len(payload)/10 {
				t.Errorf("%v: %d bytes for a %d-byte payload that repeats itself", spec, buf.Len(), len(payload))
			}
			r, err := NewReader(bytes.NewReader(buf.Bytes()), f.String())
			if err != nil {
				t.Fatalf("%v: reader: %v", spec, err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			if err != nil || !bytes.Equal(got, payload) {
				t.Errorf("%v: round trip lost the payload: %v", spec, err)
			}
		}
	}
	if _, err := (Spec{}).NewWriter(io.Discard); err == nil {
		t.Error("a writer for no format")
	}
	if _, err := NewReader(strings.NewReader(""), "br"); err == nil {
		t.Error("a reader for an unknown encoding")
	}
}

func TestByEncoding(t *testing.T) {
	for enc, want := range map[string]Format{"gzip": Gzip, "x-gzip": Gzip, " GZIP ": Gzip, "deflate": Deflate, "zstd": Zstd} {
		if got, ok := ByEncoding(enc); !ok || got != want {
			t.Errorf("ByEncoding(%q) = %v, %v", enc, got, ok)
		}
	}
	for _, enc := range []string{"", "br", "identity", "compress"} {
		if _, ok := ByEncoding(enc); ok {
			t.Errorf("ByEncoding(%q) recognised something", enc)
		}
	}
}

func TestExtensions(t *testing.T) {
	cases := map[string]string{
		"report.csv.gz": "report.csv", "a/b/x.GZ": "a/b/x", "dump.zst": "dump",
		"page.gzip": "page", "raw.zz": "raw", "old.zstd": "old",
		// A tarball's abbreviation stands for the .tar underneath it.
		"logs.tgz": "logs.tar", "logs.tzst": "logs.tar", "a/LOGS.TGZ": "a/LOGS.tar",
	}
	for in, want := range cases {
		got, ok := StripExtension(in)
		if !ok || got != want {
			t.Errorf("StripExtension(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"plain.txt", ".gz", "dir/.zst", ".tgz", "gz", "archive.tar", ""} {
		if got, ok := StripExtension(in); ok || got != in {
			t.Errorf("StripExtension(%q) = %q, %v; want unchanged", in, got, ok)
		}
	}
	for f, ext := range map[Format]string{Gzip: ".gz", Deflate: ".zz", Zstd: ".zst", None: ""} {
		if f.Extension() != ext {
			t.Errorf("%v.Extension() = %q", f, f.Extension())
		}
		if f != None && !HasExtension("x"+ext) {
			t.Errorf("HasExtension(x%s) = false", ext)
		}
	}
}
