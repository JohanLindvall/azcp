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
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
)

// Exit statuses, following cp: 0 success, 1 some file could not be copied,
// 2 the command line was wrong.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	opt, err := cli.Parse(argv)
	if err != nil {
		var ue *cli.UsageError
		if errors.As(err, &ue) {
			fmt.Fprintf(os.Stderr, "%s: %v\n", cli.Program, err)
			fmt.Fprintf(os.Stderr, "Try '%s --help' for more information.\n", cli.Program)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", cli.Program, err)
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
		fmt.Fprintf(os.Stderr, "%s: %v\n", cli.Program, err)
		return exitUsage
	}
	defer closer.Close()

	// From here on every write to the terminal is arbitrated by the progress
	// display, so log records never tear a half-drawn bar.
	logx.SetGuard(prog.Guard)
	prog.Start()
	// Stop is idempotent; this covers the paths that return early.
	defer prog.Stop()

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	go hardStopOnSecondSignal(prog, opt)

	eng, err := engine.New(engine.Config{
		Options:  opt,
		Log:      logger,
		Progress: prog,
		Stdin:    os.Stdin,
	})
	if err != nil {
		prog.Stop()
		fmt.Fprintf(os.Stderr, "%s: %v\n", cli.Program, err)
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

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		var ue *cli.UsageError
		if errors.As(runErr, &ue) {
			fmt.Fprintf(os.Stderr, "%s: %v\n", cli.Program, runErr)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", cli.Program, runErr)
		return exitFail
	}

	if opt.Output == cli.OutputJSON {
		writeJSONSummary(prog, eng, opt)
	} else {
		prog.Summary(os.Stderr, opt.DryRun)
		if n := eng.Deleted(); n > 0 {
			verb := "Removed"
			if opt.DryRun {
				verb = "Would remove"
			}
			fmt.Fprintf(os.Stderr, "  %s %d destination entrie(s) the source does not have\n",
				verb, n)
		}
	}
	reportLogged(opt)

	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "%s: interrupted%s\n", cli.Program, resumeHint(prog, opt))
		return exitFail
	}
	if failed > 0 {
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
		fmt.Fprintf(os.Stderr, "%s: %d warning(s) and %d error(s) logged to %s\n",
			cli.Program, warns, errs, opt.LogFile)
	}
}

// hardStopOnSecondSignal makes a second interrupt take effect at once. The
// first one cancels the context and lets in-flight work unwind; someone who
// asks twice wants out now, and the cursor still has to come back.
func hardStopOnSecondSignal(prog *progress.Reporter, opt *cli.Options) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch // the first is also handled by NotifyContext
	<-ch
	prog.Stop()
	fmt.Fprintf(os.Stderr, "\n%s: interrupted%s\n", cli.Program, resumeHint(prog, opt))
	os.Exit(exitFail)
}

// resumeHint says how much of an interrupted run can be picked up again, and
// says nothing at all when the answer is none of it.
//
// The two directions differ, and the difference is the whole point of saying
// anything: an upload leaves its staged blocks with the service, so it can be
// continued whatever the interrupted run was given, while a download can only
// be continued into the record that --resume writes as it goes. Telling
// somebody to rerun with --resume when there is no record to continue from
// would just be a slower way of starting over.
func resumeHint(prog *progress.Reporter, opt *cli.Options) string {
	uploads, downloads := prog.Unfinished()
	n := uploads + downloads
	switch {
	case n == 0:
		return ""
	case opt.Resume:
		return fmt.Sprintf("; %d unfinished transfer(s), which the same command "+
			"run again will continue", n)
	case downloads == 0:
		return fmt.Sprintf("; %d unfinished upload(s) — add --resume to carry on "+
			"from what already arrived", n)
	default:
		return fmt.Sprintf("; %d unfinished transfer(s) — with --resume they could "+
			"be carried on from rather than started over", n)
	}
}

// runBenchmark measures throughput instead of copying anything.
func runBenchmark(ctx context.Context, eng *engine.Engine, opt *cli.Options,
	prog *progress.Reporter) int {

	res, err := eng.Benchmark(ctx)
	prog.Stop()
	logx.SetGuard(nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: benchmark failed: %v\n", cli.Program, err)
		return exitFail
	}
	if opt.Output == cli.OutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return exitOK
	}
	res.Report()
	return exitOK
}

// writeJSONSummary closes a machine-readable run with one summary object.
func writeJSONSummary(prog *progress.Reporter, eng *engine.Engine, opt *cli.Options) {
	done, failed, skipped, retries, bytes, elapsed := prog.Totals()
	warns, errs := logx.Counts()
	summary := map[string]any{
		"event":           "summary",
		"version":         cli.VersionString(),
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
	if f := eng.Failures(); len(f) > 0 {
		summary["failures"] = f
	}
	if elapsed.Seconds() > 0 {
		summary["bytes_per_second"] = float64(bytes) / elapsed.Seconds()
	}
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(summary)
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
