// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/tui"
)

func runTUI(opt *cli.Options) int {
	logger, closer, err := logx.Init(logx.Config{Level: opt.LogLevel, Format: opt.LogFormat, File: opt.LogFile})
	if err != nil {
		logx.Errf("%s: %v\n", cli.Program, err)
		return exitFail
	}
	defer closer.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := tui.Run(ctx, opt, logger); err != nil {
		logx.Errf("%s: %v\n", cli.Program, err)
		return exitFail
	}
	checkForUpdates(ctx, opt, logger)
	return exitOK
}

// runRemove deletes, or by default lists, what --rm names.
func runRemove(opt *cli.Options) int {
	logger, closer, err := logx.Init(logx.Config{Level: opt.LogLevel, Format: opt.LogFormat,
		File: opt.LogFile, Color: colorSupported()})
	if err != nil {
		logx.Errf("%s: %v\n", cli.Program, err)
		return exitFail
	}
	defer closer.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	failed, err := tui.Remove(ctx, opt, logger)
	switch {
	case ctx.Err() != nil:
		logx.Errf("%s: interrupted\n", cli.Program)
		return exitFail
	case err != nil:
		// In JSON the failure is already an event; only a command-line
		// problem stays out of that stream.
		if opt.Output != cli.OutputJSON || isUsage(err) {
			logx.Errf("%s: %v\n", cli.Program, err)
		}
		if isUsage(err) {
			logx.Errf("Try '%s --help' for more information.\n", cli.Program)
		}
		return exitFail
	case failed > 0:
		return exitFail
	}
	if opt.Confirmed && !opt.DryRun {
		checkForUpdates(ctx, opt, logger)
	}
	return exitOK
}
