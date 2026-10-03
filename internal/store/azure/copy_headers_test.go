package azure

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/JohanLindvall/azcp/internal/store"
)

func TestStreamCopyDoesNotCommitAChecksumMismatch(t *testing.T) {
	var commits atomic.Int32
	data := bytes.Repeat([]byte("x"), 2<<20)
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		stamp(w)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
			return
		}
		if r.URL.Query().Get("comp") != "block" {
			commits.Add(1)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	})
	sum := md5.Sum([]byte("wrong"))
	err := s.streamCopy(context.Background(), &store.Node{URL: dst.WithPathPart("c/source"), Size: int64(len(data)), MD5: sum[:]}, dst, TransferOptions{CheckMD5: MD5Fail, BlockSize: 1 << 20})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || commits.Load() != 0 {
		t.Fatalf("copy: %v, commits %d", err, commits.Load())
	}
}

func TestCopyDoesNotFallBackAfterAPreconditionFailure(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprint(headers), func(t *testing.T) {
			var copies, updates atomic.Int32
			s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
				stamp(w)
				switch {
				case r.Method == http.MethodPut && r.Header.Get("x-ms-copy-source") != "":
					copies.Add(1)
					if !headers {
						refuse(w, http.StatusPreconditionFailed, "ConditionNotMet")
						return
					}
					w.Header().Set("x-ms-copy-status", "success")
					w.WriteHeader(http.StatusAccepted)
				case r.URL.Query().Get("comp") == "properties":
					updates.Add(1)
					refuse(w, http.StatusPreconditionFailed, "ConditionNotMet")
				default:
					t.Errorf("fell back after a version conflict: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			if headers {
				s.noCopyRoute.Store(dst.ServiceURL()+"|"+string(routeSync), true)
			}
			err := s.Copy(context.Background(), &store.Node{URL: dst.WithPathPart("c/source"), Size: 8}, dst, TransferOptions{ContentType: "text/plain"})
			var response *azcore.ResponseError
			if !errors.As(err, &response) || response.StatusCode != http.StatusPreconditionFailed || copies.Load() != 1 {
				t.Fatalf("copy: %v, copies %d", err, copies.Load())
			}
			if headers && updates.Load() != 1 {
				t.Fatalf("updates = %d", updates.Load())
			}
		})
	}
}

func TestEveryCopyRouteKeepsTheSourceChecksum(t *testing.T) {
	for _, route := range []string{"single", "staged", "stream"} {
		t.Run(route, func(t *testing.T) {
			var committed atomic.Int32
			sum := []byte("0123456789abcdef")
			s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
				stamp(w)
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte("contents"))
					return
				}
				if r.URL.Query().Get("comp") != "block" {
					committed.Add(1)
					if got := r.Header.Get("x-ms-blob-content-md5"); got != base64.StdEncoding.EncodeToString(sum) {
						t.Errorf("checksum = %q", got)
					}
				}
				w.WriteHeader(http.StatusCreated)
			})
			src := &store.Node{URL: dst.WithPathPart("c/source"), Size: 8, MD5: sum}
			var err error
			if route == "stream" {
				err = s.streamCopy(context.Background(), src, dst, TransferOptions{})
			} else {
				if route == "staged" {
					src.Size = maxSyncCopyBytes + 1
				}
				err = s.serverCopy(context.Background(), src, blobURL(src.URL), nil, dst, TransferOptions{BlockSize: maxSyncCopyBytes})
			}
			if err != nil {
				t.Fatal(err)
			}
			if committed.Load() != 1 {
				t.Fatalf("commits = %d", committed.Load())
			}
		})
	}
}

func TestEncodedChecksumUpdatePinsTheUploadedVersion(t *testing.T) {
	var updates atomic.Int32
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		stamp(w)
		if r.URL.Query().Get("comp") == "properties" {
			updates.Add(1)
			if r.Header.Get("If-Match") != `"0x1"` {
				t.Error("checksum update could modify a replacement blob")
			}
			refuse(w, http.StatusPreconditionFailed, "ConditionNotMet")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	})
	data := bytes.Repeat([]byte("x"), 2<<20)
	err := s.UploadEncoded(context.Background(), plain(data), int64(len(data)), dst, TransferOptions{PutMD5: true, BlockSize: 1 << 20})
	if err == nil || updates.Load() != 1 {
		t.Fatalf("update: %v (%d requests)", err, updates.Load())
	}
}

func TestAttributesOnlyCannotTruncateAConcurrentlyCreatedBlob(t *testing.T) {
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		stamp(w)
		if r.Method == http.MethodHead {
			refuse(w, http.StatusNotFound, "BlobNotFound")
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			t.Error("creation was unconditional")
		}
		refuse(w, http.StatusPreconditionFailed, "ConditionNotMet")
	})
	if err := s.PutAttrs(context.Background(), dst, TransferOptions{}); err == nil {
		t.Fatal("concurrent creation went unnoticed")
	}
}

func TestAsyncCopyDoesNotAdoptOrOverwriteAnotherCopy(t *testing.T) {
	var writes atomic.Int32
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		stamp(w)
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("x-ms-copy-status", "success")
			w.Header().Set("x-ms-copy-id", "someone-elses-copy")
		case http.MethodPut:
			writes.Add(1)
			w.Header().Set("x-ms-copy-status", "pending")
			w.Header().Set("x-ms-copy-id", "our-copy")
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("fell back after the destination changed: %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	s.noCopyRoute.Store(dst.ServiceURL()+"|"+string(routeSync), true)
	err := s.Copy(context.Background(), &store.Node{URL: dst.WithPathPart("c/source"), Size: 8}, dst, TransferOptions{})
	if !errors.Is(err, errCopyReplaced) || writes.Load() != 1 {
		t.Fatalf("copy: %v, writes %d", err, writes.Load())
	}
}

func TestAsyncCopyAppliesHeaderOverrides(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "immediate"
		if pending {
			name = "polled"
		}
		t.Run(name, func(t *testing.T) {
			var updates atomic.Int32
			sum := []byte("0123456789abcdef")
			s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
				stamp(w)
				switch {
				case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "properties":
					updates.Add(1)
					for key, want := range map[string]string{
						"If-Match":                   `"0x1"`,
						"x-ms-blob-content-type":     "text/plain",
						"x-ms-blob-content-language": "sv",
						"x-ms-blob-content-encoding": "gzip",
						"x-ms-blob-content-md5":      base64.StdEncoding.EncodeToString(sum),
					} {
						if got := r.Header.Get(key); got != want {
							t.Errorf("%s = %q, want %q", key, got, want)
						}
					}
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodHead:
					w.Header().Set("x-ms-copy-status", "success")
					w.Header().Set("x-ms-copy-id", "copy-one")
				case r.Method == http.MethodPut && r.Header.Get("x-ms-copy-source") != "":
					status := "success"
					if pending {
						status = "pending"
					}
					w.Header().Set("x-ms-copy-status", status)
					w.Header().Set("x-ms-copy-id", "copy-one")
					w.WriteHeader(http.StatusAccepted)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			src := &store.Node{URL: dst.WithPathPart("c/source"), Size: 10,
				ContentType: "application/octet-stream", ContentEncoding: "gzip", MD5: sum}
			if err := s.asyncCopy(context.Background(), src, dst, TransferOptions{
				ContentType: "text/plain", ContentLanguage: "sv",
			}); err != nil {
				t.Fatal(err)
			}
			if updates.Load() != 1 {
				t.Fatalf("header updates = %d, want 1", updates.Load())
			}
		})
	}
}

func TestAsyncCopyUnchangedHeadersNeedNoExtraRequest(t *testing.T) {
	var requests atomic.Int32
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		stamp(w)
		w.Header().Set("x-ms-copy-status", "success")
		w.WriteHeader(http.StatusAccepted)
	})
	src := &store.Node{URL: dst.WithPathPart("c/source"), ContentType: "text/plain"}
	if err := s.asyncCopy(context.Background(), src, dst, TransferOptions{ContentType: "text/plain"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("plain copy needed %d requests", requests.Load())
	}
}

func TestPutAttrsNoClobberProtectsExistingBlob(t *testing.T) {
	s, dst := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		stamp(w)
		if r.Method != http.MethodHead {
			t.Errorf("modified an existing blob: %s %s", r.Method, r.URL)
		}
	})
	err := s.PutAttrs(context.Background(), dst, TransferOptions{
		NoClobber: true, Metadata: map[string]string{"stage": "new"},
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("error = %v, want existing destination", err)
	}
}
