package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// isAllZeroByByte is the obvious implementation, kept as the oracle for the
// block-compare one.
func isAllZeroByByte(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func TestIsAllZero(t *testing.T) {
	// Several compare blocks long, ending on a partial one.
	big := make([]byte, 3*defaultBufSize+17)
	for _, b := range [][]byte{nil, {}, {0}, {1}, big} {
		if got, want := isAllZero(b), isAllZeroByByte(b); got != want {
			t.Errorf("isAllZero(%d bytes) = %v, want %v", len(b), got, want)
		}
	}
	for _, at := range []int{0, 1, defaultBufSize - 1, defaultBufSize, 2*defaultBufSize + 5, len(big) - 1} {
		c := slices.Clone(big)
		c[at] = 1
		if isAllZero(c) {
			t.Errorf("a non-zero byte at %d went unnoticed", at)
		}
	}
}

func BenchmarkIsAllZero(b *testing.B) {
	buf := make([]byte, defaultBufSize)
	for name, fn := range map[string]func([]byte) bool{"block-compare": isAllZero, "byte-loop": isAllZeroByByte} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(buf)))
			for b.Loop() {
				if !fn(buf) {
					b.Fatal("a zero buffer was not all zero")
				}
			}
		})
	}
}

// Every failure the store reports has to answer store.IsNotExist correctly,
// whichever shape the os package handed it over in.
func TestErrorsAnswerIsNotExist(t *testing.T) {
	s := New(slog.New(slog.DiscardHandler), false)
	missing := filepath.Join(t.TempDir(), "nope")
	u, err := uri.Parse(missing, uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), u); !store.IsNotExist(err) {
		t.Errorf("Remove of a missing file: %v", err)
	}
	if _, err := s.Stat(context.Background(), u, false); !store.IsNotExist(err) {
		t.Errorf("Stat of a missing file: %v", err)
	}
	if _, err := s.ReadDir(context.Background(), u); !store.IsNotExist(err) {
		t.Errorf("ReadDir of a missing directory: %v", err)
	}

	le := &os.LinkError{Op: "link", Old: "a", New: "b", Err: os.ErrNotExist}
	var pe *os.PathError
	if err := wrap("a", fmt.Errorf("wrapped: %w", le)); !store.IsNotExist(err) || !errors.As(err, &pe) || pe.Path != "a" {
		t.Errorf("a wrapped LinkError became %v", err)
	}
	if err := wrap("p", errors.New("plain")); !errors.As(err, &pe) || pe.Path != "p" {
		t.Errorf("a plain error was not given its path: %v", err)
	}
}
