// Command azcp copies files, locally or to and from Azure Blob Storage, with a
// command line compatible with cp.
package main

import (
	"context"
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

	prog := progress.New(progress.Config{Mode: opt.Progress, Out: os.Stderr})

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
	go hardStopOnSecondSignal(prog)

	eng := engine.New(engine.Config{
		Options:  opt,
		Log:      logger,
		Progress: prog,
		Stdin:    os.Stdin,
	})

	logger.Debug("starting", "version", cli.Version,
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

	prog.Summary(os.Stderr, opt.DryRun)
	reportLogged(opt)

	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "%s: interrupted\n", cli.Program)
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
func hardStopOnSecondSignal(prog *progress.Reporter) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch // the first is also handled by NotifyContext
	<-ch
	prog.Stop()
	fmt.Fprintf(os.Stderr, "\n%s: interrupted\n", cli.Program)
	os.Exit(exitFail)
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
