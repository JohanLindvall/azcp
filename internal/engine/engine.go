// Package engine plans and performs the copy. Planning walks the sources
// (expanding wildcards and recursing into directories) on one goroutine, which
// keeps cp's ordering rules and any interactive prompt sane, while a pool of
// workers moves the data.
package engine

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

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
	opt    *cli.Options
	log    *slog.Logger
	prog   *progress.Reporter
	local  *local.Store
	az     *azure.Store
	retry  retryx.Policy
	filter *filter
	pruner *pruner

	deleted int64

	failuresMu sync.Mutex
	failures   []Failure
	uriOK      uri.Options
	stdin      io.Reader

	failed atomic.Int64

	// hardLinks maps a source file identity to the copy that will provide the
	// destination all its other names link to. The first task to arrive for an
	// identity claims it and copies the data; later tasks wait for that copy
	// and link to it, which is what keeps files hard-linked in the source
	// hard-linked in the copy even though tasks run in parallel.
	hardLinksMu sync.Mutex
	hardLinks   map[local.FileID]*linkFuture

	// rootDev is the filesystem the top-level source being planned lives on.
	// --one-file-system compares against it to recognise a mount point.
	rootDev    uint64
	hasRootDev bool

	// visitedDirs guards against symlink loops when --dereference is in
	// effect. Only the scanner touches it, and the scanner is one goroutine.
	visitedDirs map[local.FileID]bool

	// deferredDirs holds directory attributes to apply once their contents
	// have been written; setting a read-only mode or an old mtime first would
	// break the copy or be immediately overwritten.
	deferredDirs []deferredDir

	// promptMu serialises interactive prompts and the answers to them.
	promptMu sync.Mutex
	// promptIn buffers stdin across prompts, so an answer typed (or piped)
	// ahead for the next question is not thrown away with the buffer.
	promptIn *bufio.Reader
	// cancelAll remembers that nobody is there to answer, so every later
	// prompt resolves to "leave it alone" without asking.
	cancelAll bool
}

type deferredDir struct {
	path string
	info os.FileInfo
}

// New builds an engine. A bad --include or --exclude pattern is reported here,
// before anything is transferred.
func New(cfg Config) (*Engine, error) {
	o := cfg.Options
	e := &Engine{
		opt:         o,
		log:         cfg.Log,
		prog:        cfg.Progress,
		stdin:       cfg.Stdin,
		hardLinks:   map[local.FileID]*linkFuture{},
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
	f, err := newFilter(o.Include, o.Exclude, o.NewerThan, o.OlderThan)
	if err != nil {
		return nil, cli.Usage(err)
	}
	e.filter = f
	if o.Delete {
		e.pruner = newPruner()
	}
	e.az = azure.New(azure.Config{
		Auth:            o.Auth,
		Log:             cfg.Log,
		Interactive:     isInteractive(),
		TenantID:        o.TenantID,
		MaxRetries:      int32(o.Retries - 1),
		TryTimeout:      o.Timeout,
		CreateContainer: o.CreateContainer,
		UserAgent:       cli.Program + "/" + cli.VersionString(),
		PeakRequests:    o.PeakRequests(),
		BytesPerSecond:  o.BandwidthLimit,
		// Reading each blob's metadata during a scan costs a larger listing
		// response, so it is asked for only when something will read it:
		// --preserve and --decompress need it and imply it, and
		// --copy-metadata requests it by name — which is what lets a
		// recorded symlink download as a symlink without -a, lets the time
		// filters see a preserved mtime, and lets a blob-to-blob copy carry
		// metadata on the routes the service does not copy it for.
		IncludeMetadata: e.preservesToBlob() || o.Decompress || o.CopyMetadata,
	})
	return e, nil
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

// isInteractive reports whether a sign-in prompt would actually reach somebody.
// The prompt is written to stderr, so that is the stream that has to be a
// terminal; a run with stderr redirected to a file must fail with a message
// rather than wait for an answer nobody can see.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
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

	// Deep enough that the scan is not held to the rate the transfers retire
	// work at. A queue of four per worker fills in seconds against large files,
	// and from then on nothing is examined at all — not even the files that
	// would have been skipped — so the total, the count of what was seen and
	// any estimate of when it will end all stay unknown until it is over. The
	// queue holds pointers to nodes the walk has already built, so depth costs
	// little beyond keeping them alive.
	tasks := make(chan *task, max(e.opt.Jobs*4, 8192))
	var wg sync.WaitGroup
	for range e.opt.Jobs {
		wg.Go(func() {
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
		})
	}

	e.prog.SetScanning(true)
	scanErr := e.scan(ctx, tasks)
	close(tasks)
	e.prog.SetScanning(false)
	wg.Wait()

	e.applyDeferredDirs()
	e.deleted = e.prune(ctx)

	if scanErr != nil {
		return e.failed.Load(), scanErr
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return e.failed.Load(), err
	}
	return e.failed.Load(), nil
}

// Deleted reports how many destination entries --delete removed.
func (e *Engine) Deleted() int64 { return e.deleted }

// Failure is one file that could not be copied.
type Failure struct {
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination,omitempty"`
	Error       string `json:"error"`
}

// Failures lists what went wrong, for the machine-readable summary.
func (e *Engine) Failures() []Failure {
	e.failuresMu.Lock()
	defer e.failuresMu.Unlock()
	return append([]Failure(nil), e.failures...)
}

// interrupted reports whether err is just the run being cancelled.
//
// The error alone cannot be relied on to say so. Go cancels a signal context
// with a cause of its own — "interrupt signal received" — and what comes back
// through the SDK is whatever it was given, which need not unwrap to
// context.Canceled. The context is the thing that knows.
func interrupted(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled))
}

// recordFailure keeps a failure for the summary. The list is capped: a run that
// fails on a hundred thousand files does not need all of them in memory to make
// the point.
func (e *Engine) recordFailure(f Failure) {
	e.failuresMu.Lock()
	defer e.failuresMu.Unlock()
	if len(e.failures) < 1000 {
		e.failures = append(e.failures, f)
	}
}

// runTask moves one file, retrying failures that look transient.
func (e *Engine) runTask(ctx context.Context, t *task) {
	pt := e.prog.Begin(t.display, t.src.Size, direction(t.src.URL, t.dst))
	err := retryx.Do(ctx, e.retry,
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
	if interrupted(ctx, err) {
		// Cancelling a run ends every transfer still in flight at once. None
		// of that is a failure, and counting it as one turns "stop" into a
		// screenful of things that went wrong.
		pt.Interrupted()
		return
	}
	pt.Done(err)

	if err != nil {
		e.failed.Add(1)
		e.log.Log(ctx, reportLevel(slog.LevelError), "cannot copy",
			"source", t.src.URL.Display(),
			"destination", t.dst.Display(),
			"error", brief(err))
		// The SDK's own error text is a multi-line dump of the whole exchange;
		// it is worth keeping, but only for someone who asked for detail.
		e.log.Debug("copy failure detail", "file", t.display, "error", err)
		e.recordFailure(Failure{
			Source:      t.src.URL.Display(),
			Destination: t.dst.Display(),
			Error:       brief(err),
		})
		if e.opt.Output == cli.OutputJSON {
			// One line per failure; the summary follows at the end.
			logx.Printf("%s\n", jsonLine(map[string]any{
				"event": "error", "source": t.src.URL.Display(),
				"destination": t.dst.Display(), "error": brief(err),
			}))
			return
		}
		var plain *plainError
		if errors.As(err, &plain) {
			logx.Errf("%s: %s\n", cli.Program, plain.msg)
		} else {
			logx.Errf("%s: cannot copy %s to %s: %s\n",
				cli.Program, quote(t.src.URL.Display()), quote(t.dst.Display()), brief(err))
		}
		return
	}
	if e.opt.Output == cli.OutputJSON {
		if e.opt.Verbose {
			logx.Printf("%s\n", jsonLine(map[string]any{
				"event": "copy", "source": t.src.URL.Display(),
				"destination": t.dst.Display(), "bytes": t.src.Size,
			}))
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
	slices.SortFunc(dirs, func(a, b deferredDir) int { return cmp.Compare(len(b.path), len(a.path)) })
	for _, d := range dirs {
		for _, err := range local.ApplyAttrs(d.path, d.path, d.info, e.opt.Preserve, false) {
			e.log.Warn("cannot preserve directory attributes", "path", d.path, "error", err)
		}
	}
}

func quote(s string) string { return "'" + s + "'" }

// jsonLine renders one event. A value that cannot be encoded is reported as
// such rather than silently dropped, because a machine reading this cannot ask.
func jsonLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"event":"error","error":"could not encode this record"}`
	}
	return string(b)
}

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
	e.log.Log(context.Background(), reportLevel(slog.LevelWarn), "skipped", "detail", msg)
	logx.Errf("%s: %s\n", cli.Program, msg)
}

// reportLevel is the level to log a problem at when a cp-style line is printed
// for it as well. Where the log shares the terminal the record would only
// repeat the line just above it, so it drops to debug — still there for
// --log-level=debug, and at full strength in a log file.
func reportLevel(level slog.Level) slog.Level {
	if logx.SharesTerminal() {
		return slog.LevelDebug
	}
	return level
}
