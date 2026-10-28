// SPDX-License-Identifier: MIT

package azure

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
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
		conditions := sourceConditions(n)
		if len(n.Tags) > 0 {
			// Tag edits leave the ETag unchanged. Pin the values we read too,
			// so changing a selected tag cannot race a filtered deletion.
			if err := store.ValidateBlobTags(n.Tags); err != nil {
				return err
			}
			terms := make([]string, 0, len(n.Tags))
			for k, v := range n.Tags {
				terms = append(terms, fmt.Sprintf(`"%s" = '%s'`, k, v))
			}
			slices.Sort(terms)
			predicate := strings.Join(terms, " AND ")
			conditions.ModifiedAccessConditions.IfTags = &predicate
		}
		_, err = bc.Delete(ctx, &blob.DeleteOptions{AccessConditions: conditions})
		if isNotFound(err) {
			err = notExist(n.URL, err)
		}
		return err
	})
}

// RemoveContainer deletes a container with everything in it, in one request
// rather than one per blob. The file manager lists the container in full
// first, as it does any directory, but a container is deleted whole: blobs
// written after that listing go with it, which is what deleting a container
// means. The account itself is never a candidate.
func (s *Store) RemoveContainer(ctx context.Context, u *uri.URL) error {
	if u.Container == "" || u.Key != "" {
		return errors.New("container removal requires a container")
	}
	return s.withSignIn(ctx, func() error {
		cc, err := s.containerClient(ctx, u)
		if err != nil {
			return err
		}
		_, err = cc.Delete(ctx, nil)
		if isNotFound(err) {
			return notExist(u, err)
		}
		if err == nil {
			// A later --create-container must not trust that it is still there.
			s.mu.Lock()
			delete(s.madeContainers, u.Account+"/"+u.Container)
			s.mu.Unlock()
		}
		return err
	})
}
