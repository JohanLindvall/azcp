// Command azcp copies files, locally or to and from Azure Blob Storage, with a
// command line compatible with cp.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/engine"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
)

// Exit statuses, following cp: 0 success, 1 for copy or command-line errors.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 1
)

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	opt, err := cli.Parse(argv)
	if err != nil {
		logx.Errf("%s: %v\n", cli.Program, err)
		if isUsage(err) {
			logx.Errf("Try '%s --help' for more information.\n", cli.Program)
		}
		return exitUsage
	}
	switch {
	case opt.ShowHelp:
		cli.PrintUsage(os.Stdout)
		return exitOK
	case opt.ShowVersion:
		fmt.Print(cli.VersionText())
		return exitOK
	}

	prog := progress.New(progress.Config{
		Mode:     opt.Progress,
		Out:      os.Stderr,
		Interval: opt.ProgressInterval,
	})

	logger, closer, err := logx.Init(logx.Config{
		Level:  opt.LogLevel,
		Format: opt.LogFormat,
		File:   opt.LogFile,
		Color:  colorSupported(),
	})
	if err != nil {
		logx.Errf("%s: %v\n", cli.Program, err)
		return exitUsage
	}
	defer closer.Close()

	// From here on every write to the terminal is arbitrated by the progress
	// display, so log records never tear a half-drawn bar.
	logx.SetGuard(prog.Guard)
	prog.Start()
	// Stop is idempotent; this covers the paths that return early.
	defer func() { prog.Stop(); logx.SetGuard(nil) }()

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	stopSecondSignal := hardStopOnSecondSignal(prog, opt)
	defer stopSecondSignal()

	eng, err := engine.New(engine.Config{
		Options:  opt,
		Log:      logger,
		Progress: prog,
		Stdin:    os.Stdin,
	})
	if err != nil {
		prog.Stop()
		logx.Errf("%s: %v\n", cli.Program, err)
		return exitUsage
	}

	if opt.Benchmark {
		return runBenchmark(ctx, eng, opt, prog)
	}

	logger.Debug("starting", "version", cli.VersionString(),
		"jobs", opt.Jobs, "part_size", opt.PartSize,
		"part_concurrency", opt.PartConcurrency, "retries", opt.Retries)

	failed, runErr := eng.Run(ctx)

	prog.Stop()
	logx.SetGuard(nil)

	fatal := runErr != nil && !errors.Is(runErr, context.Canceled)
	if fatal && opt.Output != cli.OutputJSON {
		logx.Errf("%s: %v\n", cli.Program, runErr)
		if isUsage(runErr) {
			return exitUsage
		}
		return exitFail
	}

	if opt.Output == cli.OutputJSON {
		var summaryErr error
		if fatal {
			summaryErr = runErr
		}
		writeJSONSummary(prog, eng, opt, summaryErr)
	} else {
		prog.Summary(os.Stderr, opt.DryRun)
		if n := eng.Deleted(); n > 0 {
			verb := "Removed"
			if opt.DryRun {
				verb = "Would remove"
			}
			logx.Errf("  %s %d destination %s the source does not have\n",
				verb, n, humanize.Plural(n, "entry", "entries"))
		}
	}
	reportLogged(opt)

	if ctx.Err() != nil {
		logx.Errf("%s: interrupted%s\n", cli.Program, resumeHint(prog, opt))
		return exitFail
	}
	if failed > 0 || fatal {
		return exitFail
	}
	return exitOK
}

// reportLogged points at the log when problems were recorded that the summary
// alone would not explain — most importantly when records went to a file.
func reportLogged(opt *cli.Options) {
	warns, errs := logx.Counts()
	if warns == 0 && errs == 0 {
		return
	}
	if opt.LogFile != "" {
		logx.Errf("%s: %d warning(s) and %d error(s) logged to %s\n",
			cli.Program, warns, errs, opt.LogFile)
	}
}

// hardStopOnSecondSignal makes a second interrupt take effect at once. The
// first one cancels the context and lets in-flight work unwind; someone who
// asks twice wants out now, and the cursor still has to come back.
func hardStopOnSecondSignal(prog *progress.Reporter, opt *cli.Options) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for range 2 {
			select {
			case <-ch:
			case <-done:
				return
			}
		}
		prog.Stop()
		logx.Errf("\n%s: interrupted%s\n", cli.Program, resumeHint(prog, opt))
		os.Exit(exitFail)
	}()
	return func() { signal.Stop(ch); close(done) }
}

// resumeHint says how much of an interrupted run can be picked up again, and
// says nothing at all when the answer is none of it.
//
// It takes two flags to carry on from where a run stopped, because they answer
// different questions. --resume finishes a file that stopped part-way; -n
// leaves alone the ones that arrived whole, which --resume knows nothing about
// — it keeps no record of a finished file, deliberately, since that is a job
// plan by another name. Naming only --resume, as this once did, promises to
// carry on and then copies everything again.
//
// The directions differ too: an upload leaves its staged blocks with the
// service, so it can be continued whatever the interrupted run was given, while
// a download can only be continued into a record --resume was already writing.
func resumeHint(prog *progress.Reporter, opt *cli.Options) string {
	uploads, downloads := prog.Unfinished()
	n := uploads + downloads
	switch {
	case n == 0:
		return ""
	case opt.Resume && skipsExisting(opt):
		return fmt.Sprintf("; %d unfinished transfer(s), which the same command "+
			"run again carries on from", n)
	case opt.Resume:
		return fmt.Sprintf("; %d unfinished transfer(s) — run the same command again "+
			"with -n as well and only what is missing is fetched", n)
	case downloads == 0:
		return fmt.Sprintf("; %d unfinished upload(s) — `--resume -n` carries on from "+
			"what already arrived rather than starting over", n)
	default:
		return fmt.Sprintf("; %d unfinished transfer(s) — `--resume -n` would carry on "+
			"from what already arrived rather than starting over", n)
	}
}

// skipsExisting reports whether the run already leaves a destination that is
// there alone, which is what keeps a resumed run from copying everything twice.
func skipsExisting(opt *cli.Options) bool {
	return opt.NoClobber || opt.Update != cli.UpdateAll
}

// runBenchmark measures throughput instead of copying anything.
func runBenchmark(ctx context.Context, eng *engine.Engine, opt *cli.Options,
	prog *progress.Reporter) int {

	res, err := eng.Benchmark(ctx)
	prog.Stop()
	logx.SetGuard(nil)
	if err != nil {
		logx.Errf("%s: benchmark failed: %v\n", cli.Program, err)
		return exitFail
	}
	if opt.Output == cli.OutputJSON {
		encoded, _ := json.Marshal(res)
		logx.Printf("%s\n", encoded)
		return exitOK
	}
	res.Report()
	return exitOK
}

// writeJSONSummary closes a machine-readable run with one summary object.
func writeJSONSummary(prog *progress.Reporter, eng *engine.Engine, opt *cli.Options, runErr error) {
	done, failed, skipped, retries, bytes, elapsed := prog.Totals()
	warns, errs := logx.Counts()
	failures := eng.Failures()
	if runErr != nil {
		// Fatal planning failures have no per-file task to count them. They
		// still belong in the same JSON stream and summary as copy failures.
		failed++
		failures = append(failures, engine.Failure{Error: runErr.Error()})
		encoded, _ := json.Marshal(map[string]string{"event": "error", "error": runErr.Error()})
		logx.Printf("%s\n", encoded)
	}
	summary := map[string]any{
		"event":           "summary",
		"version":         cli.VersionString(),
		"seen":            prog.Seen(),
		"copied":          done,
		"failed":          failed,
		"skipped":         skipped,
		"deleted":         eng.Deleted(),
		"bytes":           bytes,
		"retries":         retries,
		"elapsed_seconds": elapsed.Seconds(),
		"dry_run":         opt.DryRun,
		"warnings":        warns,
		"errors":          errs,
	}
	if len(failures) > 0 {
		summary["failures"] = failures
	}
	if elapsed.Seconds() > 0 {
		summary["bytes_per_second"] = float64(bytes) / elapsed.Seconds()
	}
	encoded, _ := json.Marshal(summary)
	logx.Printf("%s\n", encoded)
}

// isUsage reports whether a diagnostic should point at --help.
func isUsage(err error) bool {
	var ue *cli.UsageError
	return errors.As(err, &ue)
}

func colorSupported() bool {
	if _, off := os.LookupEnv("NO_COLOR"); off {
		return false
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}
