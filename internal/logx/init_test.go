package logx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A log file takes the records off the terminal: it is written without
// colour, redacted like everything else, and SharesTerminal says so, which is
// what lets the cp-style line be printed without the record repeating it.
func TestInitLogFileKeepsRecordsOffTheTerminal(t *testing.T) {
	t.Cleanup(func() {
		// Put the package back the way other tests expect it.
		_, closer, err := Init(Config{})
		if err == nil {
			_ = closer.Close()
		}
	})
	path := filepath.Join(t.TempDir(), "azcp.log")
	log, closer, err := Init(Config{Level: "info", File: path, Color: true})
	if err != nil {
		t.Fatal(err)
	}
	if SharesTerminal() {
		t.Error("a log file is reported as sharing the terminal")
	}
	log.Info("hello", "url", "https://a/b?sv=1&sig=SECRET")
	log.Debug("hidden")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	switch {
	case !strings.Contains(s, "hello"):
		t.Errorf("the record did not reach the file: %q", s)
	case strings.Contains(s, "SECRET"):
		t.Errorf("a signature reached the file: %q", s)
	case strings.Contains(s, "hidden"):
		t.Errorf("a record below the level reached the file: %q", s)
	case strings.Contains(s, "\x1b["):
		t.Errorf("colour escapes reached the file: %q", s)
	}
}

func TestInitRejectsUnknownLevelAndFormat(t *testing.T) {
	if _, _, err := Init(Config{Level: "loud"}); err == nil {
		t.Error("an unknown level was accepted")
	}
	if _, _, err := Init(Config{Format: "yaml"}); err == nil {
		t.Error("an unknown format was accepted")
	}
	if _, _, err := Init(Config{File: filepath.Join(t.TempDir(), "no", "such", "dir", "x.log")}); err == nil {
		t.Error("an unwritable log file was accepted")
	}
}
