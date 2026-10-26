// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JohanLindvall/azcp/internal/selfupdate"
)

func TestMaintenanceRunsAfterCopyAndCannotFailIt(t *testing.T) {
	t.Setenv("AZCP_NO_VERSION_CHECK", "0")
	t.Setenv("AZCP_NO_AUTO_UPDATE", "0")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "src"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := runSelfUpdate
	t.Cleanup(func() { runSelfUpdate = old })
	calls := 0
	runSelfUpdate = func(ctx context.Context, cfg selfupdate.Config) (selfupdate.Result, error) {
		calls++
		b, err := os.ReadFile(filepath.Join(dir, "dst"))
		if err != nil || string(b) != "payload" {
			t.Fatalf("checked before copy completed: %q, %v", b, err)
		}
		if !cfg.VersionCheck || cfg.Install {
			t.Fatalf("lost check-only policy: %+v", cfg)
		}
		return selfupdate.Result{}, errors.New("offline")
	}
	if code := runIn(t, dir, "--no-progress", "src", "dst"); code != exitOK {
		t.Fatalf("maintenance failure changed exit status: %d", code)
	}
	if calls != 1 {
		t.Fatalf("checks: %d", calls)
	}
	for _, args := range [][]string{
		{"--no-version-check", "src", "dst"},
		{"--dry-run", "src", "dst"},
		{"--help"}, {"--version"}, {"--unknown-option"},
		{"missing-source", "dst"}, {"src"},
	} {
		runIn(t, dir, args...)
		if calls != 1 {
			t.Fatalf("maintenance ran for %v", args)
		}
	}
}

func TestSelfUpdateCommandReportsSuccessAndFailure(t *testing.T) {
	t.Setenv("AZCP_NO_VERSION_CHECK", "1")
	t.Setenv("AZCP_NO_AUTO_UPDATE", "1")
	old := runSelfUpdate
	t.Cleanup(func() { runSelfUpdate = old })
	for _, fail := range []bool{false, true} {
		calls := 0
		runSelfUpdate = func(ctx context.Context, cfg selfupdate.Config) (selfupdate.Result, error) {
			calls++
			if !cfg.Install || !cfg.VersionCheck {
				t.Fatalf("explicit update lost to automatic opt-out: %+v", cfg)
			}
			if fail {
				return selfupdate.Result{}, errors.New("checksum mismatch")
			}
			return selfupdate.Result{Version: "v1.2.3", Installed: true}, nil
		}
		want := exitOK
		if fail {
			want = exitFail
		}
		if code := run([]string{"--self-update"}); code != want || calls != 1 {
			t.Fatalf("exit %d, calls %d; want %d, 1", code, calls, want)
		}
	}
}
