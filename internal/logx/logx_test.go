package logx

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Redaction is the one line of defence against an SDK error quoting a signed
// URL; every form of credential that can reach output must come out.
func TestRedactStripsCredentials(t *testing.T) {
	cases := []struct{ name, in, mustKeep, mustLose string }{
		{"sas signature",
			"GET https://acct.blob.core.windows.net/c/b?sv=2024-01-01&se=2026-01-01&sig=AbCd%2F123secret&sp=r",
			"se=2026-01-01", "AbCd%2F123secret"},
		{"sas signature first in query",
			"url: https://a.blob.core.windows.net/c?sig=topsecret123",
			"blob.core.windows.net", "topsecret123"},
		{"connection string key",
			"conn AccountName=acct;AccountKey=Zm9vYmFyc2VjcmV0MTIzNDU2Nzg5MA==;EndpointSuffix=x",
			"AccountName=acct", "Zm9vYmFyc2VjcmV0"},
		{"bearer token",
			"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature-material",
			"Authorization", "eyJhbGciOiJIUzI1NiJ9"},
		{"shared key signature",
			"Authorization: SharedKey acct:dGhpc2lzYXNpZ25hdHVyZTEyMw==",
			"acct", "dGhpc2lzYXNpZ25hdHVyZTEyMw"},
	}
	for _, c := range cases {
		got := Redact(c.in)
		if !strings.Contains(got, c.mustKeep) {
			t.Errorf("%s: redaction removed %q from %q", c.name, c.mustKeep, got)
		}
		if strings.Contains(got, c.mustLose) {
			t.Errorf("%s: secret %q survived: %q", c.name, c.mustLose, got)
		}
		if !strings.Contains(got, "<redacted>") {
			t.Errorf("%s: no redaction marker in %q", c.name, got)
		}
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	in := "cannot stat 'design=final.txt': No such file or directory"
	if got := Redact(in); got != in {
		t.Errorf("harmless text altered: %q", got)
	}
}

// A redaction shortens what lands on the wire; the writer must still report
// the caller's own length or the logger would see a short-write failure.
func TestGuardedWriterReportsFullLength(t *testing.T) {
	var sink strings.Builder
	w := &lockedWriter{w: &sink}
	msg := []byte("key AccountKey=supersecretvalue123 end\n")
	n, err := w.Write(msg)
	if err != nil || n != len(msg) {
		t.Fatalf("Write = %d, %v; want %d", n, err, len(msg))
	}
	if strings.Contains(sink.String(), "supersecretvalue123") {
		t.Errorf("secret reached the sink: %q", sink.String())
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":        slog.LevelWarn,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"ERROR":   slog.LevelError,
		"info":    slog.LevelInfo,
		"debug":   slog.LevelDebug,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseLevel("chatty"); err == nil {
		t.Error("unknown level accepted")
	}
}

// Attributes attached before WithGroup must not be qualified by a group opened
// after them; only later ones are.
func TestPrettyHandlerGroupQualification(t *testing.T) {
	var sink strings.Builder
	h := &prettyHandler{w: &lockedWriter{w: &sink}, level: slog.LevelDebug}
	log := slog.New(h).With("early", 1).WithGroup("g").With("late", 2)
	log.Info("message", "call", 3)
	out := sink.String()
	for _, want := range []string{" early=1", " g.late=2", " g.call=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, "g.early") {
		t.Errorf("early attribute wrongly qualified: %q", out)
	}
}

func TestPrettyHandlerQuotesAwkwardValues(t *testing.T) {
	var sink strings.Builder
	h := &prettyHandler{w: &lockedWriter{w: &sink}, level: slog.LevelDebug}
	slog.New(h).Info("m", "path", "with space", "plain", "bare")
	out := sink.String()
	if !strings.Contains(out, `path="with space"`) || !strings.Contains(out, "plain=bare") {
		t.Errorf("quoting wrong: %q", out)
	}
}

func TestCountingHandlerTallies(t *testing.T) {
	before2, before3 := counts[2].Load(), counts[3].Load()
	var sink strings.Builder
	h := &countingHandler{Handler: &prettyHandler{w: &lockedWriter{w: &sink}, level: slog.LevelWarn}}
	log := slog.New(h)
	log.Warn("w")
	log.Error("e")
	log.Log(context.Background(), slog.LevelError, "e2")
	warns, errs := Counts()
	if warns-before2 != 1 || errs-before3 != 2 {
		t.Errorf("counts moved by %d warns, %d errors; want 1, 2",
			warns-before2, errs-before3)
	}
}
