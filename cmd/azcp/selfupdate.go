// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/selfupdate"
)

var runSelfUpdate = selfupdate.Run

func checkForUpdates(ctx context.Context, opt *cli.Options, logger *slog.Logger) {
	if opt.DryRun || !opt.VersionCheck || ctx.Err() != nil {
		return
	}
	result, err := runSelfUpdate(ctx, selfupdate.Config{
		Version: cli.VersionString(), VersionCheck: opt.VersionCheck,
	})
	if err != nil {
		logger.Debug("version check", "error", err)
	}
	// Scripts retain cp's quiet stderr and JSON keeps ownership of stdout.
	// All user-facing maintenance output still goes through terminal arbitration.
	if ctx.Err() != nil || result.Version == "" || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	logx.Errf("azcp: version %s is available (running %s); run 'azcp --self-update' to install it\n", result.Version, cli.VersionString())
}

func selfUpdateCommand() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// An explicit command bypasses the automatic-check opt-out and daily cache.
	result, err := runSelfUpdate(ctx, selfupdate.Config{
		Version: cli.VersionString(), VersionCheck: true, Install: true,
	})
	if err != nil {
		logx.Errf("azcp: cannot update: %v\n", err)
		return exitFail
	}
	if result.Installed {
		logx.Printf("azcp: updated to %s\n", result.Version)
	} else {
		logx.Printf("azcp: already up to date (%s)\n", cli.VersionString())
	}
	return exitOK
}
