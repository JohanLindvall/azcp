// SPDX-License-Identifier: MIT

package azure

import (
	"context"
	"errors"
	"net/url"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/JohanLindvall/azcp/internal/uri"
)

type tagsContextKey struct{}

// WithTags requests index tags for this scan without changing a shared store's
// configuration. A TUI pane can need tags while another pane does not.
func WithTags(ctx context.Context) context.Context {
	return context.WithValue(ctx, tagsContextKey{}, true)
}

func (s *Store) includeTags(ctx context.Context) bool {
	return s.cfg.IncludeTags || ctx.Value(tagsContextKey{}) == true
}

func flattenTags(tags []*blob.Tags) map[string]string {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag != nil && tag.Key != nil && tag.Value != nil {
			out[*tag.Key] = *tag.Value
		}
	}
	return out
}

// Large tag sets fit the tag API but can exceed the upload header's limit.
func (o TransferOptions) separateTags() bool {
	n := 0
	for k, v := range o.Tags {
		n += len(url.QueryEscape(k)) + len(url.QueryEscape(v)) + 2
	}
	return n > 2000
}

func (o TransferOptions) tagHeader() map[string]string {
	if o.separateTags() {
		return nil
	}
	return o.Tags
}

// Once content was committed, a property failure must not retry the copy via
// another route: that repeats a successful write and may overwrite new data.
type afterWriteError struct{ error }

func (e *afterWriteError) Unwrap() error { return e.error }

func afterWrite(err error) error {
	if err != nil {
		return &afterWriteError{err}
	}
	return nil
}

func (s *Store) finishTags(ctx context.Context, dst *uri.URL, o TransferOptions, etag *azcore.ETag, err error) error {
	if err != nil || !o.separateTags() {
		return err
	}
	if etag == nil {
		return afterWrite(errors.New("cannot set tags: upload returned no ETag"))
	}
	bc, err := s.blobClient(ctx, dst)
	if err != nil {
		return afterWrite(err)
	}
	_, err = bc.SetTags(ctx, o.Tags, &blob.SetTagsOptions{
		BlobModifiedAccessConditions: &blob.BlobModifiedAccessConditions{IfMatch: etag},
	})
	return afterWrite(err)
}
