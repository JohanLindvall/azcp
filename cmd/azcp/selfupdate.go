// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"log/slog"
	"os"

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
		Version: cli.VersionString(), VersionCheck: opt.VersionCheck, AutoUpdate: opt.AutoUpdate,
	})
	if err != nil {
		logger.Debug("automatic update", "error", err)
	}
	// Scripts retain cp's quiet stderr and JSON keeps ownership of stdout.
	// All user-facing maintenance output still goes through terminal arbitration.
	if ctx.Err() != nil || result.Version == "" || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	switch {
	case result.Installed:
		logx.Errf("azcp: updated to %s; the next invocation will use it\n", result.Version)
	case err != nil:
		logx.Errf("azcp: version %s is available; could not install update: %v\n", result.Version, err)
	default:
		logx.Errf("azcp: version %s is available (running %s)\n", result.Version, cli.VersionString())
	}
}
