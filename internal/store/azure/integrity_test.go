package azure

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func transferServer(t *testing.T, handler http.HandlerFunc) (*Store, *uri.URL) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := uri.Parse(srv.URL+"/devstoreaccount1/c/blob", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Auth: AuthAnonymous, MaxRetries: -1}), u
}

func rangeReply(w http.ResponseWriter, r *http.Request, data []byte) {
	var start, end int
	if _, err := fmt.Sscanf(r.Header.Get("x-ms-range"), "bytes=%d-%d", &start, &end); err != nil {
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			start, end = 0, len(data)-1
		}
	}
	stamp(w)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func TestDownloadPinsEveryRangeToScannedVersion(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 2*minBlockSize)
	var requests atomic.Int32
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != `"0x1"` {
			t.Errorf("missing source version condition: %v", r.Header)
		}
		if requests.Add(1) > 1 {
			refuse(w, 412, "ConditionNotMet")
			return
		}
		rangeReply(w, r, data)
	})
	f, err := os.Create(filepath.Join(t.TempDir(), "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = s.Download(context.Background(), &store.Node{URL: u, Size: int64(len(data)), ETag: `"0x1"`}, f,
		TransferOptions{BlockSize: minBlockSize, Concurrency: 1})
	var response *azcore.ResponseError
	if !errors.As(err, &response) || response.StatusCode != 412 {
		t.Fatalf("changed blob accepted: %v", err)
	}
}

func TestChecksumFailureRemainsIncompleteAndRefetches(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 2*minBlockSize)
	bad := bytes.Repeat([]byte("b"), len(data))
	var correct atomic.Bool
	var requests atomic.Int32
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if correct.Load() {
			rangeReply(w, r, data)
		} else {
			rangeReply(w, r, bad)
		}
	})
	path := filepath.Join(t.TempDir(), "dst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum := md5.Sum(data)
	node := &store.Node{URL: u, Size: int64(len(data)), ETag: `"0x1"`, MD5: sum[:]}
	opts := TransferOptions{BlockSize: minBlockSize, Concurrency: 2, Resume: true, CheckMD5: MD5Fail}
	if err := s.Download(context.Background(), node, f, opts); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	if !IncompleteDownload(path) {
		t.Fatal("bad file looks complete to -n")
	}
	correct.Store(true)
	if err := s.Download(context.Background(), node, f, opts); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("made %d requests; want every bad range refetched", got)
	}
	if IncompleteDownload(path) {
		t.Fatal("completed copy still marked incomplete")
	}
}

func TestEmptyDownloadClearsStaleRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dst")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.WriteFile(path+ResumeSuffix, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Auth: AuthAnonymous})
	if err := s.Download(context.Background(), testNode(t, `"empty"`, 0), f, TransferOptions{Resume: true}); err != nil {
		t.Fatal(err)
	}
	if IncompleteDownload(path) {
		t.Fatal("empty download left its record")
	}
}

func TestResumeRejectsMissingTruncatedOrDifferentDestination(t *testing.T) {
	for _, change := range []string{"missing", "truncated", "other blob", "negative index", "past end", "invalid index"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dst")
			if err := os.WriteFile(path, make([]byte, 100), 0o600); err != nil {
				t.Fatal(err)
			}
			node := testNode(t, `"tag"`, 100)
			r, err := openResumeFile(path, node, 10)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.mark(0); err != nil {
				t.Fatal(err)
			}
			r.close()
			switch change {
			case "missing":
				err = os.Remove(path)
			case "truncated":
				err = os.Truncate(path, 1)
			case "other blob":
				node.URL = node.URL.WithPathPart("c/other")
			default:
				f, openErr := os.OpenFile(path+ResumeSuffix, os.O_APPEND|os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				bad := map[string]string{"negative index": "-1", "past end": "10", "invalid index": "not-a-number"}[change]
				_, err = fmt.Fprintln(f, bad)
				f.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			r, err = openResumeFile(path, node, 10)
			if err != nil {
				t.Fatal(err)
			}
			defer r.close()
			if r.has(0) {
				t.Fatal("stale ranges survived")
			}
		})
	}
}

func TestResumedUploadUsesOnlyMatchingContent(t *testing.T) {
	var mu sync.Mutex
	blocks := map[string][]byte{}
	stages, commits := 0, 0
	var copied []byte
	s, u := transferServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		stamp(w)
		switch {
		case r.Method == "GET":
			io.WriteString(w, "<BlockList><UncommittedBlocks>")
			for id, b := range blocks {
				fmt.Fprintf(w, "<Block><Name>%s</Name><Size>%d</Size></Block>", id, len(b))
			}
			io.WriteString(w, "</UncommittedBlocks></BlockList>")
		case r.URL.Query().Get("comp") == "block":
			stages++
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			blocks[r.URL.Query().Get("blockid")] = b
			w.WriteHeader(201)
		default:
			commits++
			if commits == 1 {
				refuse(w, 503, "ServerBusy")
				return
			}
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.NewDecoder(r.Body).Decode(&list); err != nil {
				t.Error(err)
			}
			for _, id := range list.Latest {
				copied = append(copied, blocks[id]...)
			}
			w.WriteHeader(201)
		}
	})
	data := append(bytes.Repeat([]byte("a"), minBlockSize), bytes.Repeat([]byte("b"), minBlockSize)...)
	o := TransferOptions{BlockSize: minBlockSize, Concurrency: 2}
	if err := s.UploadAt(context.Background(), bytes.NewReader(data), int64(len(data)), u, o); err == nil {
		t.Fatal("first commit should fail")
	}
	copy(data, bytes.Repeat([]byte("c"), minBlockSize))
	o.Resume = true
	if err := s.UploadAt(context.Background(), bytes.NewReader(data), int64(len(data)), u, o); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, copied) {
		t.Fatal("resumed upload combined different source versions")
	}
	if stages != 3 {
		t.Fatalf("staged %d blocks, want 2 initially and only the changed block on resume", stages)
	}
}

func TestCopySourceURLPreservesReservedCharacters(t *testing.T) {
	u, err := uri.Parse("azure://acct/c/a%3Fb%23c%25d%20e", uri.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(blobURL(u))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/c/a?b#c%d e" {
		t.Fatalf("wrong source URL: %s", parsed)
	}
}

func BenchmarkContentBlockID(b *testing.B) {
	data := strings.Repeat("x", defaultBlockSize)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := contentBlockID(context.Background(), strings.NewReader(data), 0, int64(len(data))); err != nil {
			b.Fatal(err)
		}
	}
}
