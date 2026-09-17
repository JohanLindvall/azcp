package cli

import (
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/codec"
)

func TestCompressOption(t *testing.T) {
	accepted := map[string]codec.Spec{
		"--compress":         {Format: codec.Gzip},
		"--compress=zstd:19": {Format: codec.Zstd, Level: 19},
		"--compress=9":       {Format: codec.Gzip, Level: 9},
		"--compress=zlib":    {Format: codec.Deflate},
	}
	for arg, want := range accepted {
		if got := mustParse(t, arg, "a", "b").Compress; got != want {
			t.Errorf("%s = %v, want %v", arg, got, want)
		}
	}
	if mustParse(t, "a", "b").Compress.On() {
		t.Error("compression is on by default")
	}

	rejected := map[string]string{
		"--compress=lzma":    `invalid argument "lzma" for '--compress': unknown compression`,
		"--compress=gzip:12": `invalid argument "gzip:12" for '--compress': invalid gzip level`,
	}
	for arg, want := range rejected {
		_, err := Parse([]string{arg, "a", "b"})
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v\n         want prefix %q", arg, err, want)
		}
	}

	// Compressing while expanding is a contradiction, and --compress already
	// says what the content encoding is.
	for _, argv := range [][]string{
		{"--compress", "--decompress", "a", "b"},
		{"--compress", "--content-encoding=br", "a", "b"},
	} {
		if _, err := Parse(argv); err == nil {
			t.Errorf("%v was accepted", argv)
		}
	}
}
