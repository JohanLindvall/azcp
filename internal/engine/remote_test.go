// SPDX-License-Identifier: MIT

package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
)

func TestDecompressionPreservesDestinationLinks(t *testing.T) {
	data := payload()
	encoded := compress(t, "gzip", data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(encoded)))
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("ETag", `"one"`)
		if r.Method != http.MethodHead {
			_, _ = w.Write(encoded)
		}
	}))
	defer srv.Close()
	for _, resume := range []bool{false, true} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("resume=%v/symlink=%v", resume, symlink), func(t *testing.T) {
				d := t.TempDir()
				original, dst := filepath.Join(d, "original"), filepath.Join(d, "copy")
				write(t, original, "old contents")
				link := os.Link
				if symlink {
					link = os.Symlink
				}
				if err := link(original, dst); err != nil {
					t.Skip(err)
				}
				before, err := os.Stat(original)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"--auth=anonymous", "--retries=1", "--decompress", srv.URL + "/devstoreaccount1/c/source.gz", dst}
				if resume {
					args = append([]string{"--resume"}, args...)
				}
				if n := run(t, d, args...); n != 0 {
					t.Fatal(n)
				}
				if !bytes.Equal([]byte(read(t, original)), data) || !bytes.Equal([]byte(read(t, dst)), data) {
					t.Fatal("link aliases did not see decompressed data")
				}
				after, err := os.Stat(dst)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("destination identity changed: %v", err)
				}
				if resume && exists(dst+azure.ResumeSuffix) {
					t.Fatal("successful expansion left a resume record")
				}
			})
		}
	}
}

func TestUnrelatedSidecarDoesNotBypassNoClobber(t *testing.T) {
	d := t.TempDir()
	dst := filepath.Join(d, "copy")
	write(t, dst, "keep")
	write(t, dst+azure.ResumeSuffix, "unrelated file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Error("skipped file was downloaded")
		}
		w.Header().Set("Content-Length", "10")
	}))
	defer srv.Close()
	if n := run(t, d, "--auth=anonymous", "-n", srv.URL+"/devstoreaccount1/c/source", dst); n != 0 {
		t.Fatal(n)
	}
	if read(t, dst) != "keep" || read(t, dst+azure.ResumeSuffix) != "unrelated file" {
		t.Fatal("no-clobber changed user data")
	}
}

func TestRemoteAttributesOnlyDoesNotCopyData(t *testing.T) {
	var metadata atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("restype") == "container":
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "123")
		case r.Method == http.MethodPut && r.URL.Query().Get("comp") == "metadata":
			metadata.Add(1)
			if got := r.Header.Get("x-ms-meta-stage"); got != "audited" {
				t.Errorf("metadata = %q", got)
			}
		default:
			t.Errorf("attributes-only issued a data operation: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	src := srv.URL + "/devstoreaccount1/c/source"
	dst := srv.URL + "/devstoreaccount1/c/destination"
	e := newEngine(t, "--auth=anonymous", "--retries=1", "--attributes-only", "--metadata=stage=audited", src, dst)
	pt := e.prog.Begin("source", 456, progress.DirRemote)
	err := e.transfer(context.Background(), &task{
		src: &store.Node{URL: mustURL(t, src), Kind: store.KindFile, Size: 456},
		dst: mustURL(t, dst),
	}, pt)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Load() != 1 {
		t.Fatalf("metadata updates = %d, want 1", metadata.Load())
	}
}

func TestIncompleteDownloadWithoutResumeOverridesNoClobber(t *testing.T) {
	data := "complete data"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Header().Set("ETag", `"one"`)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(data))
		}
	}))
	defer srv.Close()
	d := t.TempDir()
	dst := filepath.Join(d, "download")
	write(t, dst, "partial")
	write(t, dst+azure.ResumeSuffix, "azcp-resume stale record\n")
	src := srv.URL + "/devstoreaccount1/c/source"
	if n := run(t, d, "--auth=anonymous", "--retries=1", "-n", src, dst); n != 0 {
		t.Fatalf("unfinished destination was not repaired: %d failures", n)
	}
	if got := read(t, dst); got != data {
		t.Fatalf("download = %q", got)
	}
	if _, err := os.Stat(dst + azure.ResumeSuffix); !os.IsNotExist(err) {
		t.Fatalf("completion record remains: %v", err)
	}
}
