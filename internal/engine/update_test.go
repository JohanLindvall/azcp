package engine

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// -u compares the timestamps -p restores. A blob uploaded with -a carries its
// file's original mtime in metadata; measured against the moment it was
// uploaded instead, every blob looked newer than the copy already made and
// `azcp -au azure://… ./dir` copied the whole tree again on every run.
func TestUpdateOlderUsesThePreservedTimestamp(t *testing.T) {
	opt, err := cli.Parse([]string{"-au", "src", "dst"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{opt: opt, log: slog.New(slog.DiscardHandler)}
	at := func(s string) *uri.URL {
		u, err := uri.Parse(s, uri.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	original := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	uploaded := original.Add(30 * 24 * time.Hour)
	blobURL := at("azure://acct/c/report.csv")
	fileURL := at(filepath.Join(t.TempDir(), "report.csv"))

	preserved := func(mtime time.Time) *store.Node {
		return &store.Node{URL: blobURL, Size: 1, ModTime: uploaded,
			Metadata: map[string]string{store.MetaMTime: mtime.UTC().Format(time.RFC3339Nano)}}
	}
	file := &store.Node{URL: fileURL, Size: 1, ModTime: original}

	decide := func(src, dst *store.Node) bool {
		t.Helper()
		proceed, _, err := e.decideOverwrite(src, dst)
		if err != nil {
			t.Fatal(err)
		}
		return proceed
	}

	if decide(preserved(original), file) {
		t.Error("a download whose preserved mtime equals the file's was copied again")
	}
	if !decide(preserved(original.Add(time.Hour)), file) {
		t.Error("a blob whose preserved mtime is newer than the file was not copied")
	}
	if decide(file, preserved(original)) {
		t.Error("an upload over a blob preserving the same mtime was made again")
	}
	// A blob put there by something else carries no mtime of its own, so the
	// service's record of when it was written is what there is to compare.
	plain := &store.Node{URL: blobURL, Size: 1, ModTime: uploaded}
	if !decide(plain, file) {
		t.Error("a blob written after the file, with no preserved mtime, was not copied")
	}
}
