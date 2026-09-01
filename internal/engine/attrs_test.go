package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// A blob azcp did not put there carries none of its own attributes, but the
// service still knows when it was last written, which is the closest thing to a
// modification time it has and a great deal closer than the moment it happened
// to be downloaded. That fallback has to survive the blob carrying metadata of
// somebody else's, which is what deciding on "no metadata at all" got wrong.
func TestPreservedTimestampFallsBackToTheService(t *testing.T) {
	lastWritten := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)
	preserved := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		metadata map[string]string
		want     time.Time
	}{
		{"no metadata at all", nil, lastWritten},
		{"metadata, but none of ours", map[string]string{"colour": "blue"}, lastWritten},
		{"ours, alongside somebody else's", map[string]string{
			store.MetaMTime: preserved.Format(time.RFC3339Nano),
			"colour":        "blue",
		}, preserved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "blob.bin")
			write(t, path, "hello")

			opt, err := cli.Parse([]string{"--preserve=timestamps", "src", "dst"})
			if err != nil {
				t.Fatal(err)
			}
			e := &Engine{opt: opt, log: slog.New(slog.DiscardHandler)}

			src, err := uri.Parse("azure://acct/container/blob.bin", uri.Options{})
			if err != nil {
				t.Fatal(err)
			}
			dst, err := uri.Parse(path, uri.Options{})
			if err != nil {
				t.Fatal(err)
			}
			e.restoreAttrs(&task{
				src: &store.Node{URL: src, Metadata: tc.metadata, ModTime: lastWritten},
				dst: dst,
			})

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !fi.ModTime().UTC().Equal(tc.want) {
				t.Errorf("modification time = %s, want %s",
					fi.ModTime().UTC().Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// Cancelling a run ends every transfer still in flight at once, and the error
// each one comes back with cannot be trusted to say so: Go cancels a signal
// context with a cause of its own, and what returns through the SDK is what it
// was given. Treating those as failures is what turns Ctrl-C into a screenful
// of things that went wrong.
func TestInterruptedAsksTheContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()

	signalled := errors.New("interrupt signal received") // what Go's signal context reports
	if !interrupted(cancelled, signalled) {
		t.Error("a transfer that ended while the run was being cancelled was called a failure")
	}
	// Without the context to go on, an error that does unwrap is still known.
	if !interrupted(live, fmt.Errorf("reading bytes 0-99: %w", context.Canceled)) {
		t.Error("a wrapped context.Canceled was not recognised")
	}
	// Everything else is a real failure and has to stay one.
	if interrupted(live, errors.New("connection reset by peer")) {
		t.Error("a genuine failure was written off as an interruption")
	}
	if interrupted(cancelled, nil) {
		t.Error("a transfer that succeeded was called an interruption")
	}
}

// A download that stopped part-way is already the size of the whole blob —
// ranges arrive out of order — and was touched a moment ago. Nothing about it
// on disk says it is unfinished, so -n and -u skip it as a copy already made
// and the run reports success over a broken file. The record beside it is the
// only thing that knows, and it has to be believed.
func TestUnfinishedDownloadIsNotSkipped(t *testing.T) {
	for _, flag := range []string{"-n", "-u"} {
		t.Run(flag, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "half.bin")
			write(t, path, "the first half of it")

			opt, err := cli.Parse([]string{flag, "src", "dst"})
			if err != nil {
				t.Fatal(err)
			}
			e := &Engine{opt: opt, log: slog.New(slog.DiscardHandler)}

			srcURL, err := uri.Parse("azure://acct/container/half.bin", uri.Options{})
			if err != nil {
				t.Fatal(err)
			}
			dstURL, err := uri.Parse(path, uri.Options{})
			if err != nil {
				t.Fatal(err)
			}
			// The destination looks newer than the blob, as a file written a
			// moment ago does.
			src := &store.Node{URL: srcURL, Size: 100, ModTime: time.Now().Add(-time.Hour)}
			dst := &store.Node{URL: dstURL, Size: 100, ModTime: time.Now()}

			// With nothing beside it, it is a finished copy as far as anyone
			// can tell, and these options exist to leave it alone.
			proceed, _, err := e.decideOverwrite(src, dst)
			if err != nil {
				t.Fatal(err)
			}
			if proceed {
				t.Errorf("%s overwrote an existing destination", flag)
			}

			// With a record, it is this copy, stopped part-way.
			write(t, path+".azcp-part", "azcp-resume 1 etag 100 8388608\n0\n")
			proceed, backup, err := e.decideOverwrite(src, dst)
			if err != nil {
				t.Fatal(err)
			}
			if !proceed {
				t.Errorf("%s skipped a download that never finished, leaving it broken", flag)
			}
			if backup != "" {
				t.Errorf("backup = %q, want none: there is nothing worth keeping", backup)
			}
		})
	}
}
