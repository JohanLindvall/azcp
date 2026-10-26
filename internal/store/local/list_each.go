// SPDX-License-Identifier: MIT

package local

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// ReadDirEach bounds temporary listing memory for disk-usage scans. It does
// not sort: callers that need cp's ordered, complete listing use ReadDir.
func (s *Store) ReadDirEach(ctx context.Context, u *uri.URL, onError func(*uri.URL, error) error, fn func(*store.Node) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.Open(u.Path)
	if err != nil {
		return wrap(u.Path, err)
	}
	defer dir.Close()
	for {
		entries, readErr := dir.ReadDir(256)
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			child := u.Join(e.Name())
			info, err := e.Info()
			if err != nil {
				if err := onError(child, wrap(child.Path, err)); err != nil {
					return err
				}
				continue
			}
			if err := fn(s.node(child, info)); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return wrap(u.Path, readErr)
		}
	}
}
