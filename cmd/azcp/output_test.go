package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// A subprocess gives logx real stdout/stderr descriptors, including the early
// parse-error path before the logger or progress display exists.
func TestCommandProcess(t *testing.T) {
	if os.Getenv("AZCP_TEST_COMMAND_PROCESS") != "1" {
		return
	}
	i := slices.Index(os.Args, "--")
	if i < 0 {
		os.Exit(99)
	}
	os.Exit(run(os.Args[i+1:]))
}

func commandOutput(t *testing.T, dir string, args ...string) ([]byte, []byte, error) {
	t.Helper()
	c := exec.Command(os.Args[0], append([]string{"-test.run=^TestCommandProcess$", "--"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "AZCP_TEST_COMMAND_PROCESS=1")
	var out, diagnostics bytes.Buffer
	c.Stdout, c.Stderr = &out, &diagnostics
	err := c.Run()
	return out.Bytes(), diagnostics.Bytes(), err
}

func TestCredentialRedactionCoversParseErrorsAndJSONSummary(t *testing.T) {
	for _, args := range [][]string{
		{"azure://acct/c?sv=1&sig=review-secret"},
		{"--output=json", "https://invalid.example/c?sv=1&sig=review-secret", "dst"},
	} {
		out, diagnostics, err := commandOutput(t, t.TempDir(), args...)
		if err == nil {
			t.Fatal("invalid invocation succeeded")
		}
		if bytes.Contains(out, []byte("review-secret")) || bytes.Contains(diagnostics, []byte("review-secret")) {
			t.Fatalf("credential leaked: %s %s", out, diagnostics)
		}
	}
}

func TestJSONDeleteEmitsOnlyJSON(t *testing.T) {
	for _, dry := range []bool{false, true} {
		d := t.TempDir()
		for _, name := range []string{"src", "dst"} {
			if err := os.Mkdir(filepath.Join(d, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(d, "dst/extra"), []byte("extra"), 0o644); err != nil {
			t.Fatal(err)
		}
		args := []string{"-rT", "--delete", "--output=json", "src", "dst"}
		want := "remove"
		if dry {
			args = append(args, "--dry-run")
			want = "would-remove"
		}
		out, diagnostics, err := commandOutput(t, d, args...)
		if err != nil {
			t.Fatalf("run: %v, %s", err, diagnostics)
		}
		var events []string
		for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
			var event struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("non-JSON output: %s", line)
			}
			events = append(events, event.Event)
		}
		if !slices.Equal(events, []string{want, "summary"}) {
			t.Fatalf("events = %v", events)
		}
	}
}
