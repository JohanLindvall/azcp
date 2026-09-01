package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Benchmark answers the question people actually have when a copy is slow:
// whether the tool is misconfigured or the link is simply that fast. It moves
// generated data, so the local disk plays no part and the number is about the
// network and the account rather than about the filesystem the data happened to
// be on.

// BenchmarkResult is what the run measured.
type BenchmarkResult struct {
	Files                  int           `json:"files"`
	FileSize               int64         `json:"file_size"`
	Bytes                  int64         `json:"bytes"`
	Uploaded               time.Duration `json:"-"`
	Downloaded             time.Duration `json:"-"`
	UploadSeconds          float64       `json:"upload_seconds"`
	DownloadSeconds        float64       `json:"download_seconds"`
	UploadBytesPerSecond   float64       `json:"upload_bytes_per_second"`
	DownloadBytesPerSecond float64       `json:"download_bytes_per_second"`
	Jobs                   int           `json:"jobs"`
	PartSize               int64         `json:"part_size"`
}

// Benchmark uploads generated data to dst, reads it back, and removes it.
func (e *Engine) Benchmark(ctx context.Context) (*BenchmarkResult, error) {
	dst, err := uri.Parse(e.opt.Dest, e.uriOK)
	if err != nil {
		return nil, err
	}
	if !dst.IsRemote() {
		return nil, errors.New("--benchmark needs a blob destination, not a local path")
	}
	if err := e.az.MkdirAll(ctx, dst, 0); err != nil {
		return nil, err
	}

	// Random bytes, so nothing along the way can compress them and flatter the
	// result. One buffer serves every file: what is being measured is the
	// network, not memory.
	payload := make([]byte, e.opt.BenchSize)
	if _, err := rand.Read(payload); err != nil {
		return nil, err
	}
	total := int64(len(payload)) * int64(e.opt.BenchFiles)

	res := &BenchmarkResult{
		Files: e.opt.BenchFiles, FileSize: e.opt.BenchSize, Bytes: total,
		Jobs: e.opt.Jobs, PartSize: e.opt.PartSize,
	}
	names := make([]*uri.URL, e.opt.BenchFiles)
	for i := range names {
		names[i] = dst.Join(fmt.Sprintf("azcp-benchmark-%03d.bin", i))
	}
	// Whatever happens, the generated data does not stay behind.
	defer e.benchCleanup(names)

	e.prog.SetPhase("Benchmarking (upload)")
	e.prog.Plan(int64(len(names)), total)
	start := time.Now()
	if err := e.benchEach(ctx, names, progress.DirUpload,
		func(ctx context.Context, u *uri.URL, pt *progress.Task) error {
			opts := e.transferOptions()
			opts.Progress = pt.Set
			return e.az.UploadAt(ctx, bytes.NewReader(payload), int64(len(payload)),
				u, opts)
		}); err != nil {
		return nil, err
	}
	res.Uploaded = time.Since(start)

	e.prog.SetPhase("Benchmarking (download)")
	e.prog.Plan(int64(len(names)), total)
	start = time.Now()
	if err := e.benchEach(ctx, names, progress.DirDownload,
		func(ctx context.Context, u *uri.URL, pt *progress.Task) error {
			node := &store.Node{URL: u, Kind: store.KindFile, Size: int64(len(payload))}
			opts := e.transferOptions()
			opts.Progress = pt.Set
			opts.CheckMD5 = azure.MD5Off // nothing to check against; the data is generated
			return e.az.DownloadDiscard(ctx, node, opts)
		}); err != nil {
		return nil, err
	}
	res.Downloaded = time.Since(start)

	res.UploadSeconds = res.Uploaded.Seconds()
	res.DownloadSeconds = res.Downloaded.Seconds()
	if res.UploadSeconds > 0 {
		res.UploadBytesPerSecond = float64(total) / res.UploadSeconds
	}
	if res.DownloadSeconds > 0 {
		res.DownloadBytesPerSecond = float64(total) / res.DownloadSeconds
	}
	return res, nil
}

// benchEach runs one operation per file, --jobs at a time.
func (e *Engine) benchEach(ctx context.Context, names []*uri.URL, dir progress.Direction,
	op func(context.Context, *uri.URL, *progress.Task) error) error {

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		firstErr  error
		semaphore = make(chan struct{}, e.opt.Jobs)
	)
	for _, u := range names {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(u *uri.URL) {
			defer wg.Done()
			defer func() { <-semaphore }()
			pt := e.prog.Begin(u.Base(), e.opt.BenchSize, dir)
			err := op(ctx, u, pt)
			pt.Done(err)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(u)
	}
	wg.Wait()
	return firstErr
}

func (e *Engine) benchCleanup(names []*uri.URL) {
	// A fresh context: the caller's may already be cancelled, and leaving
	// gigabytes of generated data in someone's account is not acceptable.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()),
		2*time.Minute)
	defer cancel()
	var removed int
	for _, u := range names {
		if err := e.az.Remove(ctx, u); err != nil {
			if !store.IsNotExist(err) {
				e.log.Warn("cannot remove benchmark data",
					"blob", u.Display(), "error", err)
			}
			continue
		}
		removed++
	}
	e.log.Debug("benchmark data removed", "blobs", removed)
}

// Report writes the result for a person to read.
func (r *BenchmarkResult) Report() {
	logx.Printf("\n  %d files of %s — %s in each direction\n",
		r.Files, humanize.Bytes(r.FileSize), humanize.Bytes(r.Bytes))
	logx.Printf("  upload    %8s   %s\n",
		humanize.Duration(r.Uploaded), humanize.Rate(r.UploadBytesPerSecond))
	logx.Printf("  download  %8s   %s\n",
		humanize.Duration(r.Downloaded), humanize.Rate(r.DownloadBytesPerSecond))
	logx.Printf("\n  measured with --jobs=%d --part-size=%s\n",
		r.Jobs, humanize.Bytes(r.PartSize))
}
