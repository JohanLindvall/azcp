// SPDX-License-Identifier: MIT

package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func TestReadDirEachVisitsEveryEntryAndCancelsBetweenEntries(t *testing.T) {
	root := t.TempDir()
	for i := range 600 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte{1}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	u, err := uri.Parse(root, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(slog.New(slog.DiscardHandler), false)
	onError := func(_ *uri.URL, err error) error { return err }
	seen := make(map[string]bool)
	if err := s.ReadDirEach(t.Context(), u, onError, func(n *store.Node) error {
		if seen[n.Name()] || n.Size != 1 {
			t.Fatalf("duplicate or invalid entry: %+v", n)
		}
		seen[n.Name()] = true
		return nil
	}); err != nil || len(seen) != 600 {
		t.Fatalf("streamed listing: %d entries, %v", len(seen), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	count := 0
	err = s.ReadDirEach(ctx, u, onError, func(*store.Node) error {
		count++
		if count == 5 {
			cancel()
		}
		return nil
	})
	if count != 5 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing: %d entries, %v", count, err)
	}
}
