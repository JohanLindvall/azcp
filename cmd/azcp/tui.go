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
