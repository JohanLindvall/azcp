// SPDX-License-Identifier: MIT

package logx

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestTerminalSinkReceivesRedactedOutputAndPrompts(t *testing.T) {
	var received strings.Builder
	SetTerminalSink(func(s string) { received.WriteString(s) })
	t.Cleanup(func() { SetTerminalSink(nil) })
	log, closer, err := Init(Config{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			Printf("copy ?sig=SECRET\n")
			Errf("device prompt ?sig=SECRET\n")
			log.Error("error", "url", "https://account/c?sig=SECRET")
		})
	}
	wg.Wait()
	if got := received.String(); strings.Contains(got, "SECRET") || strings.Count(got, "device prompt") != 20 || strings.Count(got, "copy") != 20 {
		t.Fatalf("terminal sink lost output or exposed secrets: %q", got)
	}
}

func TestTerminalSinkDoesNotCaptureFileLogs(t *testing.T) {
	var received strings.Builder
	SetTerminalSink(func(s string) { received.WriteString(s) })
	t.Cleanup(func() { SetTerminalSink(nil) })
	path := filepath.Join(t.TempDir(), "log")
	log, closer, err := Init(Config{File: path})
	if err != nil {
		t.Fatal(err)
	}
	log.Warn("file log", "secret", "AccountKey=SECRET")
	Errf("visible prompt\n")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "file log") || strings.Contains(string(data), "SECRET") {
		t.Fatalf("file log: %q, %v", data, err)
	}
	if received.String() != "visible prompt\n" {
		t.Fatalf("sink captured file log: %q", received.String())
	}
}
