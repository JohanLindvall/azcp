package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/store"
)

func TestForcePreservesDestinationWhenSourceCannotBeOpened(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, src, "source")
	write(t, dst, "irreplaceable")
	if err := os.Chmod(src, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(src, 0o600) })
	if f, err := os.Open(src); err == nil {
		f.Close()
		t.Skip("this user can read a file without read permission")
	}
	if n := run(t, d, "-f", "src", "dst"); n != 1 {
		t.Fatalf("failed = %d, want 1", n)
	}
	if got := read(t, dst); got != "irreplaceable" {
		t.Fatalf("-f destroyed the destination after a source failure: %q", got)
	}
}

func TestForceOnlyRetriesOpeningTheDestination(t *testing.T) {
	e := newEngine(t, "-f", "src", "dst")
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"source open", &os.PathError{Op: "open", Path: "src", Err: syscall.EACCES}, false},
		{"destination open", &os.PathError{Op: "open", Path: "dst", Err: syscall.EACCES}, true},
		{"wrapped open", fmt.Errorf("copy: %w", &os.PathError{Op: "open", Path: "dst", Err: syscall.EPERM}), true},
		{"destination truncate", &os.PathError{Op: "truncate", Path: "dst", Err: syscall.EACCES}, false},
		{"destination write", &os.PathError{Op: "write", Path: "dst", Err: syscall.EACCES}, false},
		{"missing parent", &os.PathError{Op: "open", Path: "dst", Err: syscall.ENOENT}, false},
		{"unattributed permission", syscall.EACCES, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.forceRetryable(tt.err, "dst"); got != tt.want {
				t.Fatalf("forceRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
	e.opt.NoClobber = true
	if e.forceRetryable(&os.PathError{Op: "open", Path: "dst", Err: syscall.EACCES}, "dst") {
		t.Fatal("-f overrode -n")
	}
}

func TestOpenErrorNamesTheRightFileAndPreservesItsCause(t *testing.T) {
	task := &task{src: &store.Node{URL: mustURL(t, "src")}, dst: mustURL(t, "dst")}
	for _, name := range []string{"src", "dst"} {
		cause := &os.PathError{Op: "open", Path: name, Err: syscall.EMFILE}
		err := destError(task, cause)
		if !errors.Is(err, syscall.EMFILE) {
			t.Errorf("open error lost its retryable cause: %v", err)
		}
		if !strings.Contains(err.Error(), quote(name)) {
			t.Errorf("open error lost its path: %v", err)
		}
	}
}

func TestDanglingDestinationSymlink(t *testing.T) {
	t.Setenv("POSIXLY_CORRECT", "")
	if err := os.Unsetenv("POSIXLY_CORRECT"); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"", "-f", "--attributes-only", "--compress", "--remove-destination", "-b"} {
		t.Run(flag, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src"), "source")
			if err := os.Symlink("missing", filepath.Join(d, "dst.gz")); err != nil {
				t.Skip(err)
			}
			args := []string{"src", "dst.gz"}
			if flag != "" {
				args = append(args, flag)
			}
			want := int64(1)
			if flag == "--remove-destination" || flag == "-b" {
				want = 0
			}
			if n := run(t, d, args...); n != want {
				t.Fatalf("failed = %d, want %d", n, want)
			}
			if exists(filepath.Join(d, "missing")) {
				t.Fatal("copy wrote through a dangling destination symlink")
			}
		})
	}
}

func TestLiteralBraceSourceTakesPrecedence(t *testing.T) {
	d := t.TempDir()
	for _, name := range []string{"{a,b}", "a", "b"} {
		write(t, filepath.Join(d, name), name)
	}
	if n := run(t, d, "{a,b}", "literal"); n != 0 {
		t.Fatal(n)
	}
	if got := read(t, filepath.Join(d, "literal")); got != "{a,b}" {
		t.Fatal("existing brace name was expanded")
	}
	if err := os.Mkdir(filepath.Join(d, "expanded"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "--glob=always", "{a,b}", "expanded"); n != 0 {
		t.Fatal(n)
	}
	for _, name := range []string{"a", "b"} {
		if got := read(t, filepath.Join(d, "expanded", name)); got != name {
			t.Fatal("--glob=always did not expand braces")
		}
	}
}

func TestSameFileDoesNotDestroySource(t *testing.T) {
	for _, alias := range []string{"src", "hard", "sym"} {
		for _, flags := range [][]string{nil, {"--attributes-only"}, {"--remove-destination"}, {"-b"}} {
			t.Run(alias+strings.Join(flags, ""), func(t *testing.T) {
				d := t.TempDir()
				write(t, filepath.Join(d, "src"), "irreplaceable")
				if alias == "hard" {
					if err := os.Link(filepath.Join(d, "src"), filepath.Join(d, alias)); err != nil {
						t.Skip(err)
					}
				}
				if alias == "sym" {
					if err := os.Symlink("src", filepath.Join(d, alias)); err != nil {
						t.Skip(err)
					}
				}
				want := int64(1)
				if alias != "src" && len(flags) > 0 && flags[0] != "--attributes-only" {
					want = 0
				}
				if n := run(t, d, append(append([]string{}, flags...), "src", alias)...); n != want {
					t.Errorf("failed = %d, want %d", n, want)
				}
				if got := read(t, filepath.Join(d, "src")); got != "irreplaceable" {
					t.Fatalf("source changed: %q", got)
				}
			})
		}
	}
}

func TestBackupCannotDestroySource(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "file~"), "source")
	write(t, filepath.Join(d, "file"), "destination")
	if n := run(t, d, "-b", "file~", "file"); n != 1 {
		t.Fatal("destructive backup accepted")
	}
	if read(t, filepath.Join(d, "file~")) != "source" || read(t, filepath.Join(d, "file")) != "destination" {
		t.Fatal("backup changed source or destination")
	}
}

func TestFailedCopyRestoresBackupAndRetryStillKeepsIt(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	write(t, dst, "irreplaceable")
	e := newEngine(t, "-b", src, dst)
	copy := &task{
		src: &store.Node{URL: mustURL(t, src), Mode: 0o600},
		dst: mustURL(t, dst), backup: dst + "~",
	}
	// The source disappeared after planning. Failure must put the original
	// destination back, and a retry must still make the requested backup.
	if err := e.transfer(context.Background(), copy, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source error = %v", err)
	}
	if got := read(t, dst); got != "irreplaceable" || exists(dst+"~") {
		t.Fatalf("failed copy did not restore the original name: %q", got)
	}
	write(t, src, "replacement")
	if err := e.transfer(context.Background(), copy, nil); err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "replacement" || read(t, dst+"~") != "irreplaceable" {
		t.Fatal("retry lost the original destination or its backup")
	}
}

func TestCopyCollisionIsOrdered(t *testing.T) {
	for _, flag := range []string{"", "-n", "--backup=numbered"} {
		t.Run(flag, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "a/file"), strings.Repeat("first", 100000))
			write(t, filepath.Join(d, "b/file"), "second")
			if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
				t.Fatal(err)
			}
			argv := []string{"-j8", "a/file", "b/file", "dst"}
			if flag != "" {
				argv = append(argv, flag)
			}
			wantFailed := int64(0)
			if flag == "" {
				wantFailed = 1
			}
			if n := run(t, d, argv...); n != wantFailed {
				t.Fatalf("failed = %d, want %d", n, wantFailed)
			}
			want := strings.Repeat("first", 100000)
			if flag == "--backup=numbered" {
				if got := read(t, filepath.Join(d, "dst/file.~1~")); got != want {
					t.Fatal("first copy was not backed up")
				}
				want = "second"
			}
			if got := read(t, filepath.Join(d, "dst/file")); got != want {
				t.Fatal("collision corrupted output")
			}
		})
	}
}

func TestNoClobberRecognizesResumeSuffixAsAFileName(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/file.azcp-part"), "new")
	write(t, filepath.Join(d, "dst/file.azcp-part"), "old")
	if n := run(t, d, "--update=none", "src/file.azcp-part", "dst"); n != 0 {
		t.Fatal(n)
	}
	if got := read(t, filepath.Join(d, "dst/file.azcp-part")); got != "old" {
		t.Fatal("file overwritten")
	}
}

func TestDereferencedAttributesComeFromTarget(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "target"), "data")
	if err := os.Symlink("target", filepath.Join(d, "link")); err != nil {
		t.Skip(err)
	}
	mtime := time.Unix(946684800, 0)
	if err := os.Chtimes(filepath.Join(d, "target"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(d, "target"), 0o640); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-pL", "link", "out"); n != 0 {
		t.Fatal(n)
	}
	info, err := os.Stat(filepath.Join(d, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v", info.ModTime())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", info.Mode())
	}
	e := newEngine(t, "-pL", "link", "azure://acct/c/out")
	node, err := e.local.Stat(context.Background(), mustURL(t, "link"), true)
	if err != nil {
		t.Fatal(err)
	}
	p := store.DecodePosixMeta(e.uploadMetadata(node))
	if !p.MTime.Equal(mtime) {
		t.Errorf("uploaded mtime = %v", p.MTime)
	}
}

func TestSymbolicLinkKeepsRelativeText(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "source"), "data")
	if err := os.Symlink("source", filepath.Join(d, "probe")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-s", "source", "link"); n != 0 {
		t.Fatal(n)
	}
	if target, err := os.Readlink(filepath.Join(d, "link")); err != nil || target != "source" {
		t.Fatalf("target=%q err=%v", target, err)
	}
	if err := os.Mkdir(filepath.Join(d, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-s", "source", "sub/link"); n != 1 {
		t.Fatal("relative link outside cwd accepted")
	}
}

func TestDecompressionChecksFinalDestination(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "file.txt"), "keep")
	e := newEngine(t, "--decompress", "-n", "src", "dst")
	src := &store.Node{URL: mustURL(t, "azure://acct/c/file.txt.gz"), Kind: store.KindFile, ContentEncoding: "gzip"}
	out := make(chan *task, 1)
	if err := e.emit(context.Background(), src, mustURL(t, filepath.Join(d, "file.txt.gz")), out, "file"); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatal("-n failed to protect the expanded file")
	}
}

func TestRemoteNamesCannotEscapeDestination(t *testing.T) {
	for _, name := range []string{"../outside", "dir/../../outside", "dir/./file", "dir//file", ".", "/absolute"} {
		if err := checkLocalName(name); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if err := checkLocalName("dir/ordinary file"); err != nil {
		t.Fatal(err)
	}
}

func TestDryRunParentsCreatesNothing(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/sub/file"), "data")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "--dry-run", "--parents", "src/sub/file", "dst"); n != 0 {
		t.Fatal(n)
	}
	if got := tree(t, filepath.Join(d, "dst")); len(got) != 0 {
		t.Fatalf("dry-run created %v", got)
	}
}

func TestSelfDirectoryAliases(t *testing.T) {
	for _, spelling := range []string{"absolute", "parent", "symlink"} {
		t.Run(spelling, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/file"), "data")
			dst := filepath.Join(d, "src/new")
			if spelling == "parent" {
				dst = "src/../src/new"
			}
			if spelling == "symlink" {
				if err := os.Symlink("src", filepath.Join(d, "alias")); err != nil {
					t.Skip(err)
				}
				dst = "alias/new"
			}
			// Dry-run also avoids an unbounded recursive copy if the guard regresses.
			if n := run(t, d, "--dry-run", "-r", "src", dst); n != 1 {
				t.Fatalf("failed = %d, want 1", n)
			}
		})
	}
}

func TestDeleteNeverFollowsDestinationSymlinks(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/keep"), "data")
	write(t, filepath.Join(d, "outside/precious"), "irreplaceable")
	if err := os.Mkdir(filepath.Join(d, "dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(d, "dst/link")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-rLT", "--delete", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	if got := read(t, filepath.Join(d, "outside/precious")); got != "irreplaceable" {
		t.Fatal(got)
	}
	if exists(filepath.Join(d, "dst/link")) {
		t.Fatal("extra symlink remains")
	}
}

func TestDeleteProtectsExcludedSubtree(t *testing.T) {
	for _, exclude := range []string{"cache", "cache/**"} {
		t.Run(exclude, func(t *testing.T) {
			d := t.TempDir()
			write(t, filepath.Join(d, "src/keep"), "data")
			write(t, filepath.Join(d, "dst/cache/nested/precious"), "irreplaceable")
			write(t, filepath.Join(d, "dst/extra"), "remove")
			if n := run(t, d, "-rT", "--delete", "--exclude", exclude, "src", "dst"); n != 0 {
				t.Fatal(n)
			}
			if got := read(t, filepath.Join(d, "dst/cache/nested/precious")); got != "irreplaceable" {
				t.Fatal(got)
			}
			if exists(filepath.Join(d, "dst/extra")) {
				t.Fatal("extra file remains")
			}
		})
	}
}

func TestDereferenceCopiesAliasesButRejectsCycles(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "src/real/file"), "data")
	if err := os.Symlink("real", filepath.Join(d, "src/alias")); err != nil {
		t.Skip(err)
	}
	if n := run(t, d, "-rL", "src", "dst"); n != 0 {
		t.Fatal(n)
	}
	for _, name := range []string{"real", "alias"} {
		if got := read(t, filepath.Join(d, "dst", name, "file")); got != "data" {
			t.Fatal(got)
		}
	}
	if err := os.Symlink("..", filepath.Join(d, "src/real/loop")); err != nil {
		t.Fatal(err)
	}
	if n := run(t, d, "-rL", "src", "cycle"); n == 0 {
		t.Fatal("cycle reported success")
	}
}

func TestPartialDestinationQueuedOnlyOnce(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "file"), "partial")
	write(t, filepath.Join(d, "file.azcp-part"), "azcp-resume stale record\n")
	e := newEngine(t, "-n", "--resume", "src", "dst")
	src := &store.Node{URL: mustURL(t, "azure://acct/c/file"), Kind: store.KindFile}
	dst := mustURL(t, filepath.Join(d, "file"))
	out := make(chan *task, 2)
	for range 2 {
		if err := e.emit(context.Background(), src, dst, out, "file"); err != nil {
			t.Fatal(err)
		}
	}
	if len(out) != 1 {
		t.Fatalf("queued %d writes to one partial file", len(out))
	}
}
