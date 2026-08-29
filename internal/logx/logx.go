// Package logx configures structured logging for the tool and, crucially,
// arbitrates access to the terminal. Log records and progress bars share one
// output device, so every write goes through a guard that the progress renderer
// installs to clear its live region first.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes the desired logging setup.
type Config struct {
	Level  string // "error", "warn", "info", "debug"; empty means "warn"
	Format string // "text" or "json"; empty means "text"
	File   string // when set, records go here instead of stderr
	Color  bool
}

var (
	// sharesTerminal records that log records go to the same stderr the
	// cp-style messages do, so callers can avoid saying everything twice.
	sharesTerminal atomic.Bool

	guard   atomic.Pointer[func(func())]
	counts  [4]atomic.Int64 // indexed by levelIndex
	stdout  io.Writer       = os.Stdout
	stderrW io.Writer       = os.Stderr
	writeMu sync.Mutex
)

// SetGuard installs a function that runs its argument at a moment when the
// terminal is safe to write to. The progress renderer calls this at startup and
// clears it on shutdown.
func SetGuard(g func(func())) {
	if g == nil {
		guard.Store(nil)
		return
	}
	guard.Store(&g)
}

// WithTerminal runs fn with exclusive, display-safe access to the terminal.
func WithTerminal(fn func()) {
	if g := guard.Load(); g != nil {
		(*g)(fn)
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	fn()
}

// Printf writes a line of ordinary program output (what `cp -v` prints) to
// stdout, coordinated with the progress display.
func Printf(format string, args ...any) {
	msg := Redact(fmt.Sprintf(format, args...))
	WithTerminal(func() { io.WriteString(stdout, msg) })
}

// Errf writes a line of diagnostic output to stderr, coordinated with the
// progress display. It is used for the `cp`-style "azcp: ..." messages that are
// part of the command's contract rather than part of the log stream.
func Errf(format string, args ...any) {
	msg := Redact(fmt.Sprintf(format, args...))
	WithTerminal(func() { io.WriteString(stderrW, msg) })
}

// Secrets can reach output from places we do not control — an SDK error that
// quotes a signed URL, a connection string echoed back in a message. Rather
// than hunting every such site, everything this package writes passes through
// one redactor.
//
// Only the parts that actually grant access are removed: a SAS signature, an
// account key, a bearer token. The rest of a signed URL (its version, expiry
// and permissions) stays visible, because that is exactly what someone
// debugging a 403 needs to see.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)((?:\?|&|^)sig=)[^&\s"'<>]+`),
	regexp.MustCompile(`(?i)(AccountKey=)[^;\s"'<>]+`),
	regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._~+/-]{16,}=*`),
	regexp.MustCompile(`(?i)(SharedKey\s+[A-Za-z0-9]+:)[A-Za-z0-9+/=]{16,}`),
}

// Redact removes credential material from a string.
func Redact(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "${1}<redacted>")
	}
	return s
}

func redactBytes(p []byte) []byte {
	for _, re := range secretPatterns {
		p = re.ReplaceAll(p, []byte("${1}<redacted>"))
	}
	return p
}

func levelIndex(l slog.Level) int {
	switch {
	case l >= slog.LevelError:
		return 3
	case l >= slog.LevelWarn:
		return 2
	case l >= slog.LevelInfo:
		return 1
	default:
		return 0
	}
}

// SharesTerminal reports whether log records land on the same stream as the
// cp-style diagnostics. When they do, a problem that already has a user-facing
// message should be logged at debug level so it is not printed twice.
func SharesTerminal() bool { return sharesTerminal.Load() }

// Counts returns how many records were emitted at warn and error level. The CLI
// uses this to tell the user that something went wrong even when the transfer
// as a whole succeeded.
func Counts() (warns, errs int64) { return counts[2].Load(), counts[3].Load() }

// ParseLevel maps a level name to a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want error, warn, info or debug)", s)
}

// Init builds the logger. The returned closer flushes and closes a log file if
// one was opened; it is safe to call when no file was used.
func Init(cfg Config) (*slog.Logger, io.Closer, error) {
	lvl, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, nopCloser{}, err
	}

	var dest io.Writer = &guardedWriter{w: os.Stderr}
	sharesTerminal.Store(true)
	closer := io.Closer(nopCloser{})
	if cfg.File != "" {
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nopCloser{}, fmt.Errorf("open log file: %w", err)
		}
		// A file is not the terminal, so it needs no guard, but it does need
		// its own serialisation.
		dest = &lockedWriter{w: f}
		sharesTerminal.Store(false)
		closer = f
		cfg.Color = false
	}

	var h slog.Handler
	switch strings.ToLower(cfg.Format) {
	case "json":
		h = slog.NewJSONHandler(dest, &slog.HandlerOptions{Level: lvl})
	case "", "text", "auto":
		h = &prettyHandler{w: dest, level: lvl, color: cfg.Color}
	default:
		return nil, closer, fmt.Errorf("unknown log format %q (want text or json)", cfg.Format)
	}
	return slog.New(&countingHandler{Handler: h}), closer, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// guardedWriter routes writes through the progress renderer.
type guardedWriter struct{ w io.Writer }

func (g *guardedWriter) Write(p []byte) (int, error) {
	clean := redactBytes(p)
	var err error
	WithTerminal(func() { _, err = g.w.Write(clean) })
	// Report the caller's own length: a redaction shortens the bytes written,
	// and a short count would look like a write failure.
	return len(p), err
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(redactBytes(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// countingHandler tallies records by level so the CLI can report that problems
// were logged even if the overall exit status is zero.
type countingHandler struct{ slog.Handler }

func (c *countingHandler) Handle(ctx context.Context, r slog.Record) error {
	counts[levelIndex(r.Level)].Add(1)
	return c.Handler.Handle(ctx, r)
}

func (c *countingHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &countingHandler{Handler: c.Handler.WithAttrs(a)}
}

func (c *countingHandler) WithGroup(n string) slog.Handler {
	return &countingHandler{Handler: c.Handler.WithGroup(n)}
}

// ---------------------------------------------------------------------------
// prettyHandler: one compact line per record, tuned for reading in a terminal.
// ---------------------------------------------------------------------------

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[1;31m"
	ansiYellow = "\x1b[1;33m"
	ansiBlue   = "\x1b[1;34m"
	ansiGray   = "\x1b[1;90m"
)

type prettyHandler struct {
	w     io.Writer
	level slog.Level
	color bool
	// attrs keeps each attribute with the groups that were open when it was
	// added: an attribute attached before WithGroup must not be qualified by
	// a group opened after it.
	attrs  []boundAttr
	groups []string
}

type boundAttr struct {
	attr   slog.Attr
	groups []string
}

func (h *prettyHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *prettyHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]boundAttr(nil), h.attrs...)
	for _, a := range as {
		c.attrs = append(c.attrs, boundAttr{attr: a, groups: h.groups})
	}
	return &c
}

func (h *prettyHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

func (h *prettyHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.Grow(160)

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	h.paint(&b, ansiDim, ts.Format("15:04:05.000"))
	b.WriteByte(' ')

	name, colour := levelStyle(r.Level)
	h.paint(&b, colour, name)
	b.WriteByte(' ')
	b.WriteString(r.Message)

	for _, a := range h.attrs {
		h.appendAttr(&b, a.attr, a.groups)
	}
	r.Attrs(func(a slog.Attr) bool {
		h.appendAttr(&b, a, h.groups)
		return true
	})
	b.WriteByte('\n')
	_, err := io.WriteString(h.w, b.String())
	return err
}

func levelStyle(l slog.Level) (string, string) {
	switch {
	case l >= slog.LevelError:
		return "ERROR", ansiRed
	case l >= slog.LevelWarn:
		return "WARN ", ansiYellow
	case l >= slog.LevelInfo:
		return "INFO ", ansiBlue
	default:
		return "DEBUG", ansiGray
	}
}

func (h *prettyHandler) paint(b *strings.Builder, colour, s string) {
	if h.color {
		b.WriteString(colour)
		b.WriteString(s)
		b.WriteString(ansiReset)
		return
	}
	b.WriteString(s)
}

func (h *prettyHandler) appendAttr(b *strings.Builder, a slog.Attr, groups []string) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := a.Value.Group()
		if len(sub) == 0 {
			return
		}
		next := groups
		if a.Key != "" {
			next = append(append([]string(nil), groups...), a.Key)
		}
		for _, s := range sub {
			h.appendAttr(b, s, next)
		}
		return
	}
	b.WriteByte(' ')
	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	h.paint(b, ansiDim, key+"=")
	b.WriteString(quoteIfNeeded(a.Value.String()))
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"\\") {
		return strconv.Quote(s)
	}
	return s
}
