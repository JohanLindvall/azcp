package engine

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/progress"
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
	e := New(Config{
		Options:  opt,
		Log:      slog.New(slog.DiscardHandler),
		Progress: progress.New(progress.Config{Mode: progress.ModeNever}),
		Stdin:    strings.NewReader(""),
	})
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
			out = append(out, rel)
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
	if fi.Mode().Perm() != 0o741 {
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
