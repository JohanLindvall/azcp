package engine

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JohanLindvall/azcp/internal/codec"
)

// expand reads a compressed file back through codec, as --decompress would.
func expand(t *testing.T, path, encoding string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := codec.NewReader(f, encoding)
	if err != nil {
		t.Fatalf("%s is not %s: %v", path, encoding, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// --compress writes each file in the chosen form under its name plus the
// extension, leaves one that already is compressed alone, and keeps the
// directories as they are.
func TestCompressLocalTree(t *testing.T) {
	d := t.TempDir()
	text := strings.Repeat("alpha beta gamma\n", 2000)
	write(t, filepath.Join(d, "src/a.txt"), text)
	write(t, filepath.Join(d, "src/b.gz"), "already compressed, as far as the name says")
	write(t, filepath.Join(d, "src/sub/c.log"), "log line\n")

	// As cp does, a recursive copy to a name that does not exist yet copies
	// the directory as that name.
	if n := run(t, d, "-r", "--compress=zstd:3", "src", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	want := []string{"a.txt.zst", "b.gz", "sub", "sub/c.log.zst"}
	if got := tree(t, filepath.Join(d, "dst")); !slices.Equal(got, want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
	if got := expand(t, filepath.Join(d, "dst/a.txt.zst"), "zstd"); got != text {
		t.Error("a.txt did not survive the round trip")
	}
	if got := read(t, filepath.Join(d, "dst/b.gz")); got != "already compressed, as far as the name says" {
		t.Error("a .gz was wrapped a second time")
	}
	if got := expand(t, filepath.Join(d, "dst/sub/c.log.zst"), "zstd"); got != "log line\n" {
		t.Error("sub/c.log did not survive the round trip")
	}
	if info, err := os.Stat(filepath.Join(d, "dst/a.txt.zst")); err != nil || info.Size() >= int64(len(text))/10 {
		t.Errorf("a.txt.zst is %d bytes for %d of repeating text", info.Size(), len(text))
	}
}

// The overwrite rules, an explicit -T name and --delete all see the name that
// lands, not the source's.
func TestCompressedNameIsTheOneTheRulesSee(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a.txt"), "fresh")
	write(t, filepath.Join(d, "dst/a.txt.gz"), "keep")

	if n := run(t, d, "-n", "--compress", "src/a.txt", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if read(t, filepath.Join(d, "dst/a.txt.gz")) != "keep" {
		t.Error("-n did not protect the compressed name")
	}
	if exists(filepath.Join(d, "dst/a.txt.gz.gz")) {
		t.Error("the extension was appended to a name that had it")
	}

	if n := run(t, d, "-T", "--compress", "src/a.txt", "out.gz"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if exists(filepath.Join(d, "out.gz.gz")) || expand(t, filepath.Join(d, "out.gz"), "gzip") != "fresh" {
		t.Error("a destination already named with the extension was renamed")
	}

	// --delete keeps what the source provides under the compressed name —
	// were the plain name recorded instead, the copy just written would be
	// removed as an extra — and removes the rest, on a first run and on a
	// second one where -u leaves everything alone.
	write(t, filepath.Join(d, "mirror/src/stale"), "x")
	for _, argv := range [][]string{
		{"-r", "--delete", "--compress", "src", "mirror"},
		{"-r", "-u", "--delete", "--compress", "src", "mirror"},
	} {
		if n := run(t, d, argv...); n != 0 {
			t.Fatalf("%v: failed = %d", argv, n)
		}
		if !exists(filepath.Join(d, "mirror/src/a.txt.gz")) {
			t.Errorf("%v: --delete removed the compressed copy the source still provides", argv)
		}
		if exists(filepath.Join(d, "mirror/src/stale")) {
			t.Errorf("%v: --delete kept an entry the source does not have", argv)
		}
		write(t, filepath.Join(d, "mirror/src/stale"), "x")
	}
}

func TestCompressedTreeDestinationCollision(t *testing.T) {
	for _, flag := range []string{"", "-n", "--backup=numbered", "--dry-run"} {
		t.Run(flag, func(t *testing.T) {
			d := t.TempDir()
			text := strings.Repeat("first source\n", 10000)
			write(t, filepath.Join(d, "src/a"), text)
			write(t, filepath.Join(d, "src/a.gz"), "second source")
			args := []string{"-r", "--compress", "-j8", "src", "dst"}
			if flag != "" {
				args = append(args, flag)
			}
			wantFailed := int64(1)
			if flag == "-n" || flag == "--backup=numbered" {
				wantFailed = 0
			}
			if n := run(t, d, args...); n != wantFailed {
				t.Fatalf("failed = %d, want %d", n, wantFailed)
			}
			if flag == "--dry-run" {
				if exists(filepath.Join(d, "dst")) {
					t.Fatal("dry run created a destination")
				}
				return
			}
			encoded := filepath.Join(d, "dst/a.gz")
			if flag == "--backup=numbered" {
				if got := read(t, encoded); got != "second source" {
					t.Fatalf("last source = %q", got)
				}
				encoded += ".~1~"
			}
			if got := expand(t, encoded, "gzip"); got != text {
				t.Fatal("colliding source corrupted the compressed copy")
			}
		})
	}
}
