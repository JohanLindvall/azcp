// Package engine plans and performs the copy. Planning walks the sources
// (expanding wildcards and recursing into directories) on one goroutine, which
// keeps cp's ordering rules and any interactive prompt sane, while a pool of
// workers moves the data.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/retryx"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/azure"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// Config wires the engine to the rest of the program.
type Config struct {
	Options  *cli.Options
	Log      *slog.Logger
	Progress *progress.Reporter
	// Stdin is where an interactive overwrite prompt is read from.
	Stdin io.Reader
}

// Engine performs one invocation's worth of copying.
type Engine struct {
	opt   *cli.Options
	log   *slog.Logger
	prog  *progress.Reporter
	local *local.Store
	az    *azure.Store
	retry retryx.Policy
	uriOK uri.Options
	stdin io.Reader

	failed  atomic.Int64
	skipped atomic.Int64

	// hardLinks maps a source file identity to the first destination written
	// for it, so that files hard-linked in the source stay linked in the copy.
	hardLinksMu sync.Mutex
	hardLinks   map[local.FileID]string

	// visitedDirs guards against symlink loops when --dereference is in
	// effect. Only the scanner touches it, and the scanner is one goroutine.
	visitedDirs map[local.FileID]bool

	// deferredDirs holds directory attributes to apply once their contents
	// have been written; setting a read-only mode or an old mtime first would
	// break the copy or be immediately overwritten.
	deferredDirs []deferredDir

	// promptMu serialises interactive prompts and the answers to them.
	promptMu sync.Mutex
	// answerAll remembers a "yes to everything" style response.
	cancelAll bool
}

type deferredDir struct {
	path string
	info os.FileInfo
}

// New builds an engine.
func New(cfg Config) *Engine {
	o := cfg.Options
	e := &Engine{
		opt:         o,
		log:         cfg.Log,
		prog:        cfg.Progress,
		stdin:       cfg.Stdin,
		hardLinks:   map[local.FileID]string{},
		visitedDirs: map[local.FileID]bool{},
		retry: retryx.Policy{
			MaxAttempts: maxWholeFileAttempts(o.Retries),
			BaseDelay:   o.RetryDelay,
			MaxDelay:    o.RetryMaxDelay,
		},
		uriOK: uri.Options{EndpointSuffix: o.EndpointSuffix},
	}
	e.local = local.New(cfg.Log, o.DerefWalk())
	e.local.OneFileSystem = o.OneFileSystem
	e.az = azure.New(azure.Config{
		Auth:            o.Auth,
		Log:             cfg.Log,
		Interactive:     isInteractive(),
		TenantID:        o.TenantID,
		MaxRetries:      int32(o.Retries - 1),
		TryTimeout:      o.Timeout,
		CreateContainer: o.CreateContainer,
		UserAgent:       cli.Program + "/" + cli.Version,
	})
	return e
}

// maxWholeFileAttempts bounds how many times a whole file is restarted. The SDK
// pipeline already retries each HTTP request --retries times, so this layer
// only needs to cover failures that survive that, such as a stream that cannot
// be resumed. Keeping it small stops the two layers multiplying.
func maxWholeFileAttempts(retries int) int {
	if retries <= 1 {
		return 1
	}
	return min(3, retries)
}

func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// storeFor picks the namespace a location belongs to.
func (e *Engine) storeFor(u *uri.URL) store.Store {
	if u.IsRemote() {
		return e.az
	}
	return e.local
}

// task is one file (or symlink) to transfer.
type task struct {
	src     *store.Node
	dst     *uri.URL
	display string
	// backup, when set, is the path the existing destination is moved to first.
	backup string
	// removeFirst deletes the destination before writing, for --force and
	// --remove-destination.
	removeFirst bool
}

// Run performs the copy and reports how many files failed.
func (e *Engine) Run(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	tasks := make(chan *task, e.opt.Jobs*4)
	var wg sync.WaitGroup
	for i := 0; i < e.opt.Jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range tasks {
				if ctx.Err() != nil {
					// Drain without working, so the scanner is never blocked
					// on a channel nobody is reading.
					continue
				}
				e.runTask(ctx, t)
				if e.opt.MaxErrors > 0 && e.failed.Load() >= int64(e.opt.MaxErrors) {
					e.log.Error("too many failures, stopping",
						"failed", e.failed.Load(), "limit", e.opt.MaxErrors)
					cancel()
				}
			}
		}()
	}

	e.prog.SetScanning(true)
	scanErr := e.scan(ctx, tasks)
	close(tasks)
	e.prog.SetScanning(false)
	wg.Wait()

	e.applyDeferredDirs()

	if scanErr != nil {
		return e.failed.Load(), scanErr
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return e.failed.Load(), err
	}
	return e.failed.Load(), nil
}

// Skipped reports how many files were deliberately not copied.
func (e *Engine) Skipped() int64 { return e.skipped.Load() }

// runTask moves one file, retrying failures that look transient.
func (e *Engine) runTask(ctx context.Context, t *task) {
	pt := e.prog.Begin(t.display, t.src.Size, direction(t.src.URL, t.dst))
	var err error
	attemptErr := retryx.Do(ctx, e.retry,
		func(attempt int, delay time.Duration, cause error) {
			pt.Retrying(attempt, e.retry.MaxAttempts, delay)
			e.log.Warn("transfer failed, retrying",
				"file", t.display,
				"source", t.src.URL.Display(),
				"destination", t.dst.Display(),
				"attempt", attempt,
				"retry_in", delay.String(),
				"cause", retryx.Describe(cause),
				"error", cause)
		},
		func(ctx context.Context) error {
			pt.Resumed()
			pt.Set(0)
			return e.transfer(ctx, t, pt)
		})
	err = attemptErr
	pt.Done(err)

	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		e.failed.Add(1)
		level := slog.LevelError
		if logx.SharesTerminal() {
			// The cp-style line below already tells the user; keep the
			// structured record for --log-level=debug and for log files.
			level = slog.LevelDebug
		}
		e.log.Log(ctx, level, "cannot copy",
			"source", t.src.URL.Display(),
			"destination", t.dst.Display(),
			"error", brief(err))
		// The SDK's own error text is a multi-line dump of the whole exchange;
		// it is worth keeping, but only for someone who asked for detail.
		e.log.Debug("copy failure detail", "file", t.display, "error", err)
		var plain *plainError
		if errors.As(err, &plain) {
			logx.Errf("%s: %s\n", cli.Program, plain.msg)
		} else {
			logx.Errf("%s: cannot copy %s to %s: %s\n",
				cli.Program, quote(t.src.URL.Display()), quote(t.dst.Display()), brief(err))
		}
		return
	}
	if e.opt.Verbose {
		logx.Printf("%s -> %s\n", quote(t.src.URL.Display()), quote(t.dst.Display()))
	}
}

func direction(src, dst *uri.URL) progress.Direction {
	switch {
	case src.IsRemote() && dst.IsRemote():
		return progress.DirRemote
	case dst.IsRemote():
		return progress.DirUpload
	case src.IsRemote():
		return progress.DirDownload
	default:
		return progress.DirLocal
	}
}

// applyDeferredDirs sets directory attributes after their contents are in
// place, deepest first so that a read-only parent does not block a child.
func (e *Engine) applyDeferredDirs() {
	if len(e.deferredDirs) == 0 {
		return
	}
	dirs := e.deferredDirs
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].path) > len(dirs[j].path) })
	for _, d := range dirs {
		for _, err := range local.ApplyAttrs(d.path, d.path, d.info, e.opt.Preserve, false) {
			e.log.Warn("cannot preserve directory attributes", "path", d.path, "error", err)
		}
	}
}

func quote(s string) string { return "'" + s + "'" }

// brief renders an error for a single-line report. Azure failures collapse to
// their status and error code; everything else keeps its own wording.
func brief(err error) string { return retryx.Describe(err) }

// plainError carries a message that is already a complete cp-style report, so
// the failure path prints it as-is instead of wrapping it in another sentence.
type plainError struct{ msg string }

func (e *plainError) Error() string { return e.msg }

func plainf(format string, args ...any) error {
	return &plainError{fmt.Sprintf(format, args...)}
}

// note reports an operational problem that does not stop the copy. Like fail
// it prints one cp-style line and keeps the structured record out of the way
// when the log is going to the same terminal.
func (e *Engine) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if logx.SharesTerminal() {
		e.log.Debug("skipped", "detail", msg)
	} else {
		e.log.Warn("skipped", "detail", msg)
	}
	logx.Errf("%s: %s\n", cli.Program, msg)
}
