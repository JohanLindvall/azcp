package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/azcp/internal/store"
)

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
