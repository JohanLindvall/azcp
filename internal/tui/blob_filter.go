// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/JohanLindvall/azcp/internal/glob"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
)

var blobFilterFields = []string{
	"content-type", "content-encoding", "content-language", "content-disposition",
	"cache-control", "access-tier", "etag",
}

func isBlobField(field string) bool {
	return slices.Contains(blobFilterFields, field) || strings.HasPrefix(field, "metadata.") || strings.HasPrefix(field, "tags.")
}

func (f *nameFilter) blobTerm(field, op, value string) (filterTerm, error) {
	if field == "metadata." || field == "tags." {
		return nil, fmt.Errorf("write %sKEY:VALUE or %sKEY=VALUE", field, field)
	}
	if op != ":" && op != "=" {
		return nil, fmt.Errorf("write %s:VALUE or %s=VALUE", field, field)
	}
	match, err := blobValueMatcher(value, op == "=")
	if err != nil {
		return nil, err
	}
	if !slices.Contains(f.blobFields, field) {
		f.blobFields = append(f.blobFields, field)
	}
	return func(e entry, _ bool) (bool, bool) {
		var value string
		var present bool
		if e.usage != nil {
			value, present = e.usage.blob.get(field)
		} else {
			value, present = blobValue(e.node, field)
		}
		return present && match(value), true
	}, nil
}

func blobValueMatcher(value string, exact bool) (func(string) bool, error) {
	if exact {
		// An empty metadata value is distinct from a missing key.
		return func(s string) bool { return s == value }, nil
	}
	if value == "" {
		return nil, errors.New("nothing to match")
	}
	if !glob.HasMeta(value) && !strings.Contains(value, `\`) {
		value = strings.ToLower(value)
		return func(s string) bool { return strings.Contains(strings.ToLower(s), value) }, nil
	}
	expanded, err := glob.ExpandBraces(value)
	if err != nil {
		return nil, err
	}
	patterns := make([]glob.Segment, 0, len(expanded))
	for _, alt := range expanded {
		p, err := glob.CompileText(alt)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	return func(s string) bool {
		return slices.ContainsFunc(patterns, func(p glob.Segment) bool { return p.Match(s) })
	}, nil
}

// Prefixes, containers and local files do not inherit blob properties.
func blobValue(n *store.Node, field string) (string, bool) {
	if n == nil || n.IsDir() || n.URL == nil || !n.URL.IsRemote() {
		return "", false
	}
	if key, ok := strings.CutPrefix(field, "tags."); ok {
		value, present := n.Tags[key]
		return value, present
	}
	if key, ok := strings.CutPrefix(field, "metadata."); ok {
		if value, present := n.Metadata[key]; present {
			return value, true
		}
		for k, value := range n.Metadata {
			if strings.EqualFold(k, key) {
				return value, true
			}
		}
		return "", false
	}
	var value string
	switch field {
	case "content-type":
		value = n.ContentType
	case "content-encoding":
		value = n.ContentEncoding
	case "content-language":
		value = n.ContentLanguage
	case "content-disposition":
		value = n.ContentDisposition
	case "cache-control":
		value = n.CacheControl
	case "access-tier":
		value = n.AccessTier
	case "etag":
		value = n.ETag
	}
	return value, value != ""
}

func needsTags(fields []string) bool {
	return slices.ContainsFunc(fields, func(field string) bool { return strings.HasPrefix(field, "tags.") })
}

func withBlobFields(ctx context.Context, fields []string) context.Context {
	if needsTags(fields) {
		return azure.WithTags(ctx)
	}
	return ctx
}

// Usage caches retain only fields a filter requested, without a metadata map
// or a credential-bearing URL on every node in the tree.
type blobFilterValues struct{ values []blobFilterValue }

type blobFilterValue struct{ field, value string }

func (v *blobFilterValues) get(field string) (string, bool) {
	if v != nil {
		for _, item := range v.values {
			if item.field == field {
				return item.value, true
			}
		}
	}
	return "", false
}

func cacheBlobValues(n *store.Node, fields []string) *blobFilterValues {
	var values []blobFilterValue
	for _, field := range fields {
		if value, present := blobValue(n, field); present {
			if values == nil {
				values = make([]blobFilterValue, 0, len(fields))
			}
			values = append(values, blobFilterValue{field, value})
		}
	}
	if len(values) == 0 {
		return nil
	}
	return &blobFilterValues{values: values}
}
