package engine

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// run drives a whole invocation the way main does, inside dir, and reports how
// many files failed.
func run(t *testing.T, dir string, argv ...string) int64 {
	t.Helper()
	t.Chdir(dir)
	opt, err := cli.Parse(argv)
	if err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	e, err := New(Config{
		Options:  opt,
		Log:      slog.New(slog.DiscardHandler),
		Progress: progress.New(progress.Config{Mode: progress.ModeNever}),
		Stdin:    strings.NewReader(""),
	})
	if err != nil {
		t.Fatalf("new engine %v: %v", argv, err)
	}
	failed, runErr := e.Run(context.Background())
	if runErr != nil {
		t.Fatalf("run %v: %v", argv, runErr)
	}
	return failed
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// tree lists every path under root, relative and sorted, for comparing layouts.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(root, p); rel != "." {
			// Compared against "/"-separated expectations, so Windows'
			// backslashes are normalised rather than special-cased.
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCopyFile(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a.txt"), "hello")
	if n := run(t, d, "a.txt", "b.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "b.txt")); got != "hello" {
		t.Errorf("content = %q", got)
	}
}

func TestCopyIntoDirectory(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a.txt"), "1")
	write(t, filepath.Join(d, "b.txt"), "2")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "a.txt", "b.txt", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if read(t, filepath.Join(d, "dst/a.txt")) != "1" || read(t, filepath.Join(d, "dst/b.txt")) != "2" {
		t.Error("files did not land in the directory")
	}
}

func TestRecursive(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/one.txt"), "1")
	write(t, filepath.Join(d, "src/deep/two.txt"), "2")
	if err := os.MkdirAll(filepath.Join(d, "src/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-r", "src", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	want := []string{"deep", "deep/two.txt", "empty", "one.txt"}
	got := tree(t, filepath.Join(d, "dst"))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tree = %v, want %v", got, want)
	}
}

func TestRecursiveRequiredForDirectory(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/one.txt"), "1")
	if n := run(t, d, "src", "dst"); n != 1 {
		t.Fatalf("failed = %d, want 1", n)
	}
	if exists(filepath.Join(d, "dst")) {
		t.Error("destination was created despite the error")
	}
}

func TestGlobDoubleStar(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a/x.log"), "x")
	write(t, filepath.Join(d, "src/a/b/y.log"), "y")
	write(t, filepath.Join(d, "src/a/b/z.txt"), "z")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "src/**/*.log", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if !exists(filepath.Join(d, "dst/x.log")) || !exists(filepath.Join(d, "dst/y.log")) {
		t.Error("** did not reach both logs")
	}
	if exists(filepath.Join(d, "dst/z.txt")) {
		t.Error("** matched a file the pattern excludes")
	}
}

func TestGlobExtendedAndBraces(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"keep.log", "junk.tmp", "app-1.log", "app-2.log"} {
		write(t, filepath.Join(d, "src", n), n)
	}
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "src/!(*.tmp)", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if exists(filepath.Join(d, "dst/junk.tmp")) {
		t.Error("!(*.tmp) matched the excluded file")
	}
	if !exists(filepath.Join(d, "dst/keep.log")) {
		t.Error("!(*.tmp) missed keep.log")
	}

	d2 := t.TempDir()
	write(t, filepath.Join(d2, "src/2023/a.log"), "a")
	write(t, filepath.Join(d2, "src/2024/b.log"), "b")
	write(t, filepath.Join(d2, "src/2025/c.log"), "c")
	if err := os.Mkdir(filepath.Join(d2, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d2, "src/{2023,2025}/*.log", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if !exists(filepath.Join(d2, "dst/a.log")) || !exists(filepath.Join(d2, "dst/c.log")) {
		t.Error("brace expansion missed a branch")
	}
	if exists(filepath.Join(d2, "dst/b.log")) {
		t.Error("brace expansion matched an unlisted branch")
	}
}

func TestGlobNoMatchIsAnError(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a.txt"), "a")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "src/*.nothing", "dst"); n != 1 {
		t.Fatalf("failed = %d, want 1", n)
	}
}

// A path that exists exactly as written must not be treated as a pattern.
func TestLiteralNameWithMetacharacters(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "report[final].pdf"), "pdf")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "report[final].pdf", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "dst/report[final].pdf")); got != "pdf" {
		t.Errorf("content = %q", got)
	}
}

func TestNoClobber(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a.txt"), "new")
	write(t, filepath.Join(d, "b.txt"), "old")
	if n := run(t, d, "-n", "a.txt", "b.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "b.txt")); got != "old" {
		t.Errorf("-n overwrote: %q", got)
	}
}

func TestUpdateOlder(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "a.txt"), filepath.Join(d, "b.txt")
	write(t, src, "new")
	write(t, dst, "old")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dst, future, future); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-u", "a.txt", "b.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, dst); got != "old" {
		t.Errorf("-u replaced a newer destination: %q", got)
	}

	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dst, past, past); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-u", "a.txt", "b.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, dst); got != "new" {
		t.Errorf("-u skipped an older destination: %q", got)
	}
}

func TestBackup(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a.txt"), "new")
	write(t, filepath.Join(d, "b.txt"), "old")
	if n := run(t, d, "-b", "a.txt", "b.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "b.txt")); got != "new" {
		t.Errorf("destination = %q", got)
	}
	if got := read(t, filepath.Join(d, "b.txt~")); got != "old" {
		t.Errorf("backup = %q", got)
	}
}

func TestSymlinkHandlingMatchesCp(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/file.txt"), "content")
	if err := os.Symlink("file.txt", filepath.Join(d, "src/link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Recursive: links are copied as links.
	if n := run(t, d, "-r", "src", "rec"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	fi, err := os.Lstat(filepath.Join(d, "rec/link.txt"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("recursive copy did not preserve the symlink: %v", fi.Mode())
	}
	// Non-recursive: the link is followed.
	if n := run(t, d, "src/link.txt", "plain.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	fi, err = os.Lstat(filepath.Join(d, "plain.txt"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Errorf("non-recursive copy did not dereference: %v", fi.Mode())
	}
	if got := read(t, filepath.Join(d, "plain.txt")); got != "content" {
		t.Errorf("dereferenced content = %q", got)
	}
	// -L follows during recursion too.
	if n := run(t, d, "-rL", "src", "deref"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	fi, _ = os.Lstat(filepath.Join(d, "deref/link.txt"))
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("-L did not dereference during recursion")
	}
}

func TestPreserveTimestampsAndMode(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "a.txt")
	write(t, src, "x")
	if err := os.Chmod(src, 0o741); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(src, when, when); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-p", "a.txt", "b.txt"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	fi, err := os.Stat(filepath.Join(d, "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX mode: Go maps the whole thing to one read-only bit,
	// so there is nothing meaningful to compare.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o741 {
		t.Errorf("mode = %v, want 0741", fi.Mode().Perm())
	}
	if !fi.ModTime().Truncate(time.Second).Equal(when) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), when)
	}
}

func TestParents(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "a/b/c.txt"), "deep")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "--parents", "a/b/c.txt", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "dst/a/b/c.txt")); got != "deep" {
		t.Errorf("--parents put the file elsewhere: %q", got)
	}
}

func TestNoTargetDirectory(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a.txt"), "1")
	if n := run(t, d, "-rT", "src", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	// -T copies the contents of src onto dst, not src into dst.
	if !exists(filepath.Join(d, "dst/a.txt")) || exists(filepath.Join(d, "dst/src")) {
		t.Errorf("-T layout wrong: %v", tree(t, filepath.Join(d, "dst")))
	}
}

func TestSelfCopyRefused(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a.txt"), "1")
	if err := os.Mkdir(filepath.Join(d, "src/inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-r", "src", "src/inner"); n != 1 {
		t.Fatalf("failed = %d, want 1", n)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a.txt"), "1")
	if n := run(t, d, "--dry-run", "-r", "src", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if exists(filepath.Join(d, "dst")) {
		t.Error("--dry-run created the destination")
	}
}

func TestParallelCopyIsComplete(t *testing.T) {
	d := t.TempDir()
	const n = 120
	want := map[string]string{}
	for i := range n {
		name := filepath.Join("src", "f"+string(rune('a'+i%26))+"-"+itoa(i)+".bin")
		body := strings.Repeat("x", 1+i*37)
		write(t, filepath.Join(d, name), body)
		want[filepath.Base(name)] = body
	}
	if got := run(t, d, "-r", "--jobs=16", "src", "dst"); got != 0 {
		t.Fatalf("failed = %d", got)
	}
	for name, body := range want {
		if got := read(t, filepath.Join(d, "dst", name)); got != body {
			t.Fatalf("%s: content mismatch (%d vs %d bytes)", name, len(got), len(body))
		}
	}
}

func itoa(i int) string {
	var b []byte
	if i == 0 {
		return "0"
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A large file exercises the chunked copy path rather than a single write.
func TestLargeFileRoundTrip(t *testing.T) {
	d := t.TempDir()
	body := bytes.Repeat([]byte("abcdefghij"), 400_000) // ~4 MiB
	if err := os.WriteFile(filepath.Join(d, "big.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "big.bin", "copy.bin"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	got, err := os.ReadFile(filepath.Join(d, "copy.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("large file differs: %d vs %d bytes", len(got), len(body))
	}
}

func TestSparseFileRoundTrip(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "sparse.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("end"), 8<<20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for _, mode := range []string{"auto", "always", "never"} {
		out := "out-" + mode + ".bin"
		if n := run(t, d, "--sparse="+mode, "sparse.bin", out); n != 0 {
			t.Fatalf("sparse=%s failed = %d", mode, n)
		}
		a, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(d, out))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("sparse=%s: content differs (%d vs %d bytes)", mode, len(a), len(b))
		}
	}
}

// A symbolic link pointing back up its own tree must not make -L loop.
func TestDereferenceSymlinkLoopTerminates(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a.txt"), "1")
	if err := os.Symlink("..", filepath.Join(d, "src/up")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	done := make(chan int64, 1)
	go func() { done <- run(t, d, "-rL", "src", "dst") }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("copy did not terminate: the symlink loop was not detected")
	}
	if !exists(filepath.Join(d, "dst/a.txt")) {
		t.Error("the real file was not copied")
	}
}

// TestOneFileSystemStopsAtMountPoints checks the -x decision directly. Creating
// a real mount point inside a temporary directory needs privileges the test
// suite does not have, so the device comparison is exercised with synthetic
// stat results instead.
func TestOneFileSystemStopsAtMountPoints(t *testing.T) {
	d := t.TempDir()
	fi, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	dev, ok := local.DeviceOf(fi.Sys())
	if !ok {
		t.Skip("device identity unavailable on this platform")
	}

	e := &Engine{opt: &cli.Options{OneFileSystem: true}, rootDev: dev, hasRootDev: true}
	sameFS := &store.Node{URL: mustURL(t, d), Kind: store.KindDir, Sys: fi.Sys()}
	if e.crossesFilesystem(sameFS) {
		t.Error("a directory on the root filesystem was treated as a mount point")
	}

	otherFS := &store.Node{URL: mustURL(t, d), Kind: store.KindDir, Sys: fi.Sys()}
	e.rootDev = dev + 1
	if !e.crossesFilesystem(otherFS) {
		t.Error("a directory on another filesystem was not recognised")
	}

	// Plain files are never skipped: only a directory can be a mount point.
	file := &store.Node{URL: mustURL(t, d), Kind: store.KindFile, Sys: fi.Sys()}
	if e.crossesFilesystem(file) {
		t.Error("-x skipped a file")
	}

	// Without the option nothing is skipped.
	e.opt.OneFileSystem = false
	if e.crossesFilesystem(otherFS) {
		t.Error("-x applied when it was not requested")
	}
}

func mustURL(t *testing.T, path string) *uri.URL {
	t.Helper()
	u, err := uri.Parse(path, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// -x must not disturb an ordinary copy that stays on one filesystem.
func TestOneFileSystemCopiesNormally(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/a/b.txt"), "1")
	if n := run(t, d, "-rx", "src", "dst"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if got := read(t, filepath.Join(d, "dst/a/b.txt")); got != "1" {
		t.Errorf("content = %q", got)
	}
}

func TestExcludeAndInclude(t *testing.T) {
	setup := func(t *testing.T) string {
		d := t.TempDir()
		for _, p := range []string{
			"src/keep.log", "src/junk.tmp", "src/notes.md",
			"src/a/deep.log", "src/a/deep.tmp",
			"src/build/out.bin", "src/build/sub/more.bin",
			"src/vendor/lib.go",
		} {
			write(t, filepath.Join(d, p), p)
		}
		return d
	}

	t.Run("a bare pattern matches the name at any depth", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-r", "--exclude", "*.tmp", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		for _, gone := range []string{"dst/junk.tmp", "dst/a/deep.tmp"} {
			if exists(filepath.Join(d, gone)) {
				t.Errorf("%s should have been excluded", gone)
			}
		}
		for _, kept := range []string{"dst/keep.log", "dst/a/deep.log", "dst/notes.md"} {
			if !exists(filepath.Join(d, kept)) {
				t.Errorf("%s should have been copied", kept)
			}
		}
	})

	// The anchor is what is being copied, not how it was named, so the same
	// pattern works however the source is spelled.
	t.Run("a pattern with a slash anchors at the copy root", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-r", "--exclude", "build/**", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if exists(filepath.Join(d, "dst/build/out.bin")) ||
			exists(filepath.Join(d, "dst/build/sub/more.bin")) {
			t.Error("the build subtree should have been pruned")
		}
		if !exists(filepath.Join(d, "dst/keep.log")) {
			t.Error("pruning took too much with it")
		}

		// Spelling the source differently must not change what the pattern means.
		d2 := setup(t)
		abs := filepath.Join(d2, "src")
		if n := run(t, d2, "-r", "--exclude", "build/**", abs, "dst2"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if exists(filepath.Join(d2, "dst2/build/out.bin")) {
			t.Error("an absolute source changed what the pattern anchored to")
		}
	})

	t.Run("include selects, and exclude beats include", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-r", "--include", "*.log", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if !exists(filepath.Join(d, "dst/keep.log")) || !exists(filepath.Join(d, "dst/a/deep.log")) {
			t.Error("--include did not keep the logs")
		}
		if exists(filepath.Join(d, "dst/notes.md")) || exists(filepath.Join(d, "dst/junk.tmp")) {
			t.Error("--include kept something it should not have")
		}

		d2 := setup(t)
		if n := run(t, d2, "-r", "--include", "*.log", "--exclude", "deep.*", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if !exists(filepath.Join(d2, "dst/keep.log")) {
			t.Error("keep.log should have survived")
		}
		if exists(filepath.Join(d2, "dst/a/deep.log")) {
			t.Error("--exclude should beat --include")
		}
	})

	t.Run("repeated flags accumulate and braces expand", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-r", "--exclude", "*.tmp", "--exclude", "*.{md,bin}", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		for _, gone := range []string{"dst/junk.tmp", "dst/notes.md", "dst/build/out.bin"} {
			if exists(filepath.Join(d, gone)) {
				t.Errorf("%s should have been excluded", gone)
			}
		}
		if !exists(filepath.Join(d, "dst/keep.log")) {
			t.Error("keep.log should have survived")
		}
	})

	t.Run("extended patterns work here too", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-r", "--include", "!(*.tmp|*.bin)", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if exists(filepath.Join(d, "dst/junk.tmp")) || exists(filepath.Join(d, "dst/build/out.bin")) {
			t.Error("the extended pattern did not exclude")
		}
		if !exists(filepath.Join(d, "dst/keep.log")) {
			t.Error("the extended pattern excluded too much")
		}
	})

	// An unclosed group is literal text to a shell, and to this matcher too;
	// it must not become an error or match everything by accident.
	t.Run("an unclosed group is literal, as it is in a shell", func(t *testing.T) {
		d := setup(t)
		write(t, filepath.Join(d, "src/@(unclosed"), "odd name")
		if n := run(t, d, "-r", "--exclude", "@(unclosed", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if exists(filepath.Join(d, "dst/@(unclosed")) {
			t.Error("the literal name was not excluded")
		}
		if !exists(filepath.Join(d, "dst/keep.log")) {
			t.Error("an unclosed group excluded more than its literal self")
		}
	})
}

func TestTimeWindowFilters(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/old.txt"), "old")
	write(t, filepath.Join(d, "src/new.txt"), "new")
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(d, "src/old.txt"), old, old); err != nil {
		t.Fatal(err)
	}

	if n := run(t, d, "-r", "--newer-than", "7d", "src", "recent"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if exists(filepath.Join(d, "recent/old.txt")) {
		t.Error("--newer-than copied something older than the window")
	}
	if !exists(filepath.Join(d, "recent/new.txt")) {
		t.Error("--newer-than skipped something inside the window")
	}

	if n := run(t, d, "-r", "--older-than", "7d", "src", "aged"); n != 0 {
		t.Fatalf("failed = %d", n)
	}
	if !exists(filepath.Join(d, "aged/old.txt")) || exists(filepath.Join(d, "aged/new.txt")) {
		t.Errorf("--older-than selected the wrong files: %v", tree(t, filepath.Join(d, "aged")))
	}
}

func TestDeleteMakesTheDestinationMatch(t *testing.T) {
	setup := func(t *testing.T) string {
		d := t.TempDir()
		write(t, filepath.Join(d, "src/keep.txt"), "keep")
		write(t, filepath.Join(d, "src/sub/nested.txt"), "nested")
		write(t, filepath.Join(d, "dst/keep.txt"), "stale")
		write(t, filepath.Join(d, "dst/stray.txt"), "stray")
		write(t, filepath.Join(d, "dst/sub/gone.txt"), "gone")
		write(t, filepath.Join(d, "dst/junk.tmp"), "junk")
		return d
	}

	t.Run("removes what the source does not have", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-rT", "--delete", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		for _, gone := range []string{"dst/stray.txt", "dst/sub/gone.txt", "dst/junk.tmp"} {
			if exists(filepath.Join(d, gone)) {
				t.Errorf("%s should have been removed", gone)
			}
		}
		for _, kept := range []string{"dst/keep.txt", "dst/sub/nested.txt"} {
			if !exists(filepath.Join(d, kept)) {
				t.Errorf("%s should have survived", kept)
			}
		}
	})

	t.Run("an excluded entry is protected, not deleted", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-rT", "--delete", "--exclude", "*.tmp", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if !exists(filepath.Join(d, "dst/junk.tmp")) {
			t.Error("--exclude should protect an entry from --delete, not mark it for removal")
		}
		if exists(filepath.Join(d, "dst/stray.txt")) {
			t.Error("stray.txt should still have been removed")
		}
	})

	t.Run("dry run removes nothing", func(t *testing.T) {
		d := setup(t)
		if n := run(t, d, "-rT", "--delete", "--dry-run", "src", "dst"); n != 0 {
			t.Fatalf("failed = %d", n)
		}
		if !exists(filepath.Join(d, "dst/stray.txt")) {
			t.Error("--dry-run deleted something")
		}
	})

	// A half-read source looks exactly like a source with fewer files in it,
	// which is precisely when deleting would be catastrophic.
	t.Run("refuses when the source could not be read in full", func(t *testing.T) {
		d := setup(t)
		locked := filepath.Join(d, "src/locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Skipf("cannot create an unreadable directory: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		n := run(t, d, "-rT", "--delete", "src", "dst")
		if n == 0 {
			t.Skip("the directory was readable anyway, most likely running as root")
		}
		if !exists(filepath.Join(d, "dst/stray.txt")) {
			t.Error("deleted despite a failure during the copy")
		}
	})

	t.Run("--delete needs -r", func(t *testing.T) {
		if _, err := cli.Parse([]string{"--delete", "a", "b"}); err == nil {
			t.Error("--delete was accepted without -r")
		}
	})
}
