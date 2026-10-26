// SPDX-License-Identifier: MIT

package azure

import (
	"context"
	"errors"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/JohanLindvall/azcp/internal/store"
)

// RemoveBlob deletes exactly the selected revision. The file manager knows
// whether it selected a blob or a prefix; falling back to a directory marker
// on 404 could remove a different object. A concurrent writer must also keep
// its new data when a move finishes copying the previous revision.
func (s *Store) RemoveBlob(ctx context.Context, n *store.Node) error {
	if n == nil || n.IsDir() || n.URL.Key == "" || n.ETag == "" {
		return errors.New("blob removal requires a file and its ETag")
	}
	return s.withSignIn(ctx, func() error {
		bc, err := s.blobClient(ctx, n.URL)
		if err != nil {
			return err
		}
		_, err = bc.Delete(ctx, &blob.DeleteOptions{AccessConditions: sourceConditions(n)})
		if isNotFound(err) {
			err = notExist(n.URL, err)
		}
		return err
	})
}
