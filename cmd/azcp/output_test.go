package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
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

func TestJSONFatalPlanningErrorEndsWithSummary(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "src"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--output=json", "-t", "src", "src"},
		{"--output=json", "--backup", "src", "azure://acct/c/blob"},
	} {
		out, diagnostics, err := commandOutput(t, d, args...)
		if err == nil {
			t.Fatal("invalid destination succeeded")
		}
		lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
		if len(lines) != 2 {
			t.Fatalf("expected error and summary, got %s; stderr: %s", out, diagnostics)
		}
		for i, name := range []string{"error", "summary"} {
			var event struct {
				Event    string            `json:"event"`
				Failed   int               `json:"failed"`
				Failures []json.RawMessage `json:"failures"`
			}
			if err := json.Unmarshal(lines[i], &event); err != nil || event.Event != name {
				t.Fatalf("invalid %s event: %s, %v", name, lines[i], err)
			}
			if name == "summary" && (event.Failed != 1 || len(event.Failures) != 1) {
				t.Fatalf("fatal failure missing from summary: %s", lines[i])
			}
		}
	}
}

// Each file in the JSON says when its source was last written, in UTC and to
// the precision the filesystem kept: under --dry-run, so that a reader can tell
// from the dry run alone whether a copy it holds is current, and with -v, so the
// record of a copy made says what it was a copy of. A local file has no content
// encoding, so none is claimed.
func TestJSONFileEventsSayWhenTheSourceWasWritten(t *testing.T) {
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(d, "src", "a.txt")
	if err := os.WriteFile(file, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	written := time.Date(2026, 9, 23, 12, 0, 0, 500_000_000, time.UTC)
	if err := os.Chtimes(file, written, written); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--dry-run", "--output=json", "-rT", "src", "dst"},
		{"-v", "--output=json", "-rT", "src", "dst"},
	} {
		out, diagnostics, err := commandOutput(t, d, args...)
		if err != nil {
			t.Fatalf("%v: %v, %s", args, err, diagnostics)
		}
		var files int
		for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatalf("non-JSON output: %s", line)
			}
			if event["event"] == "summary" {
				continue
			}
			files++
			if event["modified"] != written.Format(time.RFC3339Nano) {
				t.Errorf("%v: modified = %v, want %s", args, event["modified"], written.Format(time.RFC3339Nano))
			}
			if _, ok := event["content_encoding"]; ok {
				t.Errorf("%v: a local file claims a content encoding: %s", args, line)
			}
		}
		if files != 1 {
			t.Fatalf("%v: %d file events in %s", args, files, out)
		}
	}
}
