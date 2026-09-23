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

// A dry run's JSON is meant to be enough to decide whether a copy already made
// is current, without asking again. That holds only if each event says when
// the source was written exactly as -u compares it, to the nanosecond and in a
// zone a reader need not guess, and which encoding --decompress would undo.
func TestFileEventCarriesWhatDecidesACopy(t *testing.T) {
	opt, err := cli.Parse([]string{"-u", "src", "dst"})
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
	written := time.Date(2026, 9, 23, 15, 4, 5, 123456789, time.FixedZone("CEST", 2*60*60))
	blob := &store.Node{URL: at("azure://acct/c/tree/_manifest.json.zstd"), Size: 42,
		ModTime: written, ContentEncoding: "zstd"}
	dst := at(filepath.Join(t.TempDir(), "_manifest.json"))

	event := fileEvent("would-copy", blob, dst)
	if event["event"] != "would-copy" || event["bytes"] != int64(42) || event["content_encoding"] != "zstd" {
		t.Fatalf("event = %v", event)
	}
	stamp, _ := event["modified"].(string)
	modified, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("modified %q: %v", stamp, err)
	}
	if !modified.Equal(written) || modified.Location() != time.UTC {
		t.Fatalf("modified = %s, want %s in UTC", stamp, written.UTC().Format(time.RFC3339Nano))
	}

	// What a reader concludes from the event is what -u concludes.
	for _, c := range []struct {
		file time.Time
		copy bool
	}{
		{modified, false},
		{modified.Add(-time.Nanosecond), true},
		{modified.Add(time.Second), false},
	} {
		proceed, _, err := e.decideOverwrite(blob, &store.Node{URL: dst, Size: 1, ModTime: c.file})
		if err != nil {
			t.Fatal(err)
		}
		if proceed != c.copy {
			t.Errorf("file written at %s: -u copies = %v, but the event says %s",
				c.file.Format(time.RFC3339Nano), proceed, stamp)
		}
	}

	// A blob that carries its file's own mtime is compared on that by -u, so
	// that is the time the event gives.
	original := written.Add(-48 * time.Hour)
	blob.Metadata = map[string]string{store.MetaMTime: original.UTC().Format(time.RFC3339Nano)}
	if got := fileEvent("would-copy", blob, dst)["modified"]; got != original.UTC().Format(time.RFC3339Nano) {
		t.Errorf("with a preserved mtime, modified = %v, want %s", got, original.UTC().Format(time.RFC3339Nano))
	}

	// A source that has neither says nothing, rather than a zero time or an
	// empty encoding a reader might take at its word.
	bare := fileEvent("copy", &store.Node{URL: at(filepath.Join(t.TempDir(), "x")), Size: 1}, dst)
	for _, key := range []string{"modified", "content_encoding"} {
		if _, ok := bare[key]; ok {
			t.Errorf("a source without it has %s = %v", key, bare[key])
		}
	}
}
