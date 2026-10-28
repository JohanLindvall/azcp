// SPDX-License-Identifier: MIT

package azure

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
)

func TestEveryWriteRouteSendsTags(t *testing.T) {
	for _, large := range []bool{false, true} {
		for _, route := range []string{"upload", "blocks", "resume", "encoded", "encoded-blocks", "stream", "url", "url-blocks", "async", "attributes-create"} {
			t.Run(fmt.Sprintf("%s/large=%v", route, large), func(t *testing.T) {
				var writes, sets atomic.Int32
				wanted := map[string]string{"Project": "alpha + beta", "project": "lower", "empty": ""}
				if large {
					for k := range wanted {
						wanted[k] = strings.Repeat("+", 256)
					}
				}
				s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
					stamp(w)
					if r.Method == http.MethodPut && r.URL.Query().Get("comp") == "tags" {
						sets.Add(1)
						if !large || writes.Load() != 1 || r.Header.Get("x-ms-blob-if-match") != `"0x1"` {
							t.Error("tags did not follow the committed revision")
						}
						data, _ := io.ReadAll(r.Body)
						var body struct {
							Tags []struct{ Key, Value string } `xml:"TagSet>Tag"`
						}
						if err := xml.Unmarshal(data, &body); err != nil {
							t.Error(err)
						}
						got := map[string]string{}
						for _, tag := range body.Tags {
							got[tag.Key] = tag.Value
						}
						if !maps.Equal(got, wanted) {
							t.Error("large tags were lost")
						}
						w.WriteHeader(http.StatusNoContent)
						return
					}
					switch r.Method {
					case http.MethodHead:
						refuse(w, 404, "BlobNotFound")
						return
					case http.MethodGet:
						if r.URL.Query().Get("comp") == "blocklist" {
							_, _ = io.WriteString(w, "<BlockList><UncommittedBlocks/></BlockList>")
							return
						}
						_, _ = io.WriteString(w, "payload")
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					if r.URL.Query().Get("comp") != "block" {
						writes.Add(1)
						tags, err := url.ParseQuery(r.Header.Get("x-ms-tags"))
						if large && len(tags) != 0 {
							t.Error("large tags still used the upload header")
						}
						if !large && (err != nil || len(tags) != 3 || tags.Get("Project") != "alpha + beta" || tags.Get("project") != "lower" || !tags.Has("empty")) {
							t.Errorf("lost tags: %v (%v)", tags, err)
						}
					}
					w.Header().Set("x-ms-copy-status", "success")
					if route == "async" {
						w.WriteHeader(http.StatusAccepted)
					} else {
						w.WriteHeader(http.StatusCreated)
					}
				})
				o := TransferOptions{Tags: wanted, BlockSize: 1 << 20, Concurrency: 2}
				src := &store.Node{URL: dst.WithPathPart("c/source"), Size: 7}
				var err error
				switch route {
				case "upload", "blocks", "resume":
					data := []byte("payload")
					if route != "upload" {
						data = bytes.Repeat([]byte("x"), 2<<20)
					}
					path := filepath.Join(t.TempDir(), "file")
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					o.Resume = route == "resume"
					err = s.Upload(t.Context(), path, dst, o)
				case "encoded", "encoded-blocks":
					size := int64(7)
					if route == "encoded-blocks" {
						size = 2 << 20
					}
					err = s.UploadEncoded(t.Context(), func() io.ReadCloser { return io.NopCloser(strings.NewReader("payload")) }, size, dst, o)
				case "stream":
					err = s.streamCopy(t.Context(), src, dst, o)
				case "url", "url-blocks":
					if route == "url-blocks" {
						src.Size, o.BlockSize = maxSyncCopyBytes+1, maxSyncCopyBytes
					}
					err = s.serverCopy(t.Context(), src, "https://source.blob.core.windows.net/c/source", nil, dst, o)
				case "async":
					err = s.asyncCopy(t.Context(), src, dst, o)
				case "attributes-create":
					err = s.PutAttrs(t.Context(), dst, o)
				}
				if (sets.Load() == 1) != large {
					t.Errorf("tag updates = %d, large=%v", sets.Load(), large)
				}
				if err != nil || writes.Load() != 1 {
					t.Fatalf("write: %v, commits=%d", err, writes.Load())
				}
			})
		}
	}
}

func TestTagUpdatesPinTheLatestDestinationETag(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint(clear), func(t *testing.T) {
			var sets atomic.Int32
			s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
				stamp(w)
				switch r.URL.Query().Get("comp") {
				case "metadata":
					w.Header().Set("ETag", `"metadata"`)
				case "properties":
					if r.Header.Get("If-Match") != `"metadata"` {
						t.Error("headers lost metadata revision")
					}
					w.Header().Set("ETag", `"headers"`)
				case "tags":
					sets.Add(1)
					if r.Header.Get("x-ms-blob-if-match") != `"headers"` {
						t.Error("tags could overwrite a concurrently replaced blob")
					}
					body, _ := io.ReadAll(r.Body)
					w.WriteHeader(http.StatusNoContent)
					if strings.Contains(string(body), "<Key>") == clear {
						t.Errorf("tag body = %s", body)
					}
				}
			})
			tags := map[string]string{}
			if !clear {
				tags["Project"] = "alpha"
			}
			err := s.PutAttrs(t.Context(), dst, TransferOptions{Tags: tags, Metadata: map[string]string{"a": "b"}, ContentType: "text/plain"})
			if err != nil || sets.Load() != 1 {
				t.Fatalf("update: %v, sets=%d", err, sets.Load())
			}
		})
	}
}

func TestTagsAreOptInAndListingsNeedNoPerBlobReads(t *testing.T) {
	var tagReads, lists atomic.Int32
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		stamp(w)
		switch r.URL.Query().Get("comp") {
		case "tags":
			tagReads.Add(1)
			if r.Header.Get("x-ms-blob-if-match") != `"0x1"` {
				t.Error("tag read lost blob revision")
			}
			_, _ = io.WriteString(w, `<Tags><TagSet><Tag><Key>Project</Key><Value>alpha</Value></Tag><Tag><Key>empty</Key><Value></Value></Tag></TagSet></Tags>`)
		case "list":
			lists.Add(1)
			tags := ""
			if strings.Contains(r.URL.Query().Get("include"), "tags") {
				tags = `<Tags><TagSet><Tag><Key>Project</Key><Value>alpha</Value></Tag><Tag><Key>empty</Key><Value></Value></Tag></TagSet></Tags>`
			}
			_, _ = fmt.Fprintf(w, `<EnumerationResults><Blobs><Blob><Name>file</Name><Properties><Content-Length>1</Content-Length></Properties>%s</Blob></Blobs><NextMarker/></EnumerationResults>`, tags)
		default:
			w.Header().Set("Content-Length", "1")
		}
	})
	for _, include := range []bool{false, true} {
		ctx := t.Context()
		if include {
			ctx = WithTags(ctx)
		}
		n, err := s.Stat(ctx, u, false)
		if err != nil || (n.Tags["Project"] == "alpha") != include {
			t.Fatalf("stat tags: %v, %v", n, err)
		}
		base := u.WithPathPart("c")
		nodes, err := s.ReadDirEntries(ctx, base)
		if err != nil || len(nodes) != 1 || (nodes[0].Tags["Project"] == "alpha") != include {
			t.Fatalf("list tags: %v, %v", nodes, err)
		}
		err = s.WalkAll(ctx, base, nil, func(n *store.Node) error {
			if !n.IsDir() && (n.Tags["Project"] == "alpha") != include {
				t.Error("flat listing lost tags")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if tagReads.Load() != 1 || lists.Load() != 4 {
		t.Fatalf("tag reads=%d, lists=%d", tagReads.Load(), lists.Load())
	}
	if s.includeTags(context.Background()) {
		t.Fatal("scan changed shared store configuration")
	}
}

func TestRemovalPinsTagsAsWellAsContent(t *testing.T) {
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != `"original"` || r.Header.Get("x-ms-if-tags") != `"Project" = 'alpha' AND "empty" = ''` {
			t.Error("removal did not protect the selected content and tag values")
		}
		refuse(w, http.StatusPreconditionFailed, "ConditionNotMet")
	})
	err := s.RemoveBlob(t.Context(), &store.Node{URL: u, Kind: store.KindFile, ETag: `"original"`, Tags: map[string]string{"Project": "alpha", "empty": ""}})
	if err == nil {
		t.Fatal("tag changes were ignored")
	}
}

func TestTagFailureAfterCommitDoesNotCopyAgain(t *testing.T) {
	var writes atomic.Int32
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("comp") == "tags" {
			refuse(w, http.StatusForbidden, "AuthorizationPermissionMismatch")
			return
		}
		writes.Add(1)
		stamp(w)
		w.Header().Set("x-ms-copy-status", "success")
		if r.Header.Get("x-ms-blob-type") != "" {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	})
	tags := map[string]string{"a": strings.Repeat("+", 256), "b": strings.Repeat("+", 256), "c": strings.Repeat("+", 256)}
	err := s.Copy(t.Context(), &store.Node{URL: dst.WithPathPart("c/source"), Size: 7}, dst, TransferOptions{Tags: tags})
	if err == nil || writes.Load() != 1 {
		t.Fatalf("post-commit failure: %v, writes=%d", err, writes.Load())
	}
}
