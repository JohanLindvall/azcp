// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/engine"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/store"
)

// List prints the entries of each operand, or what opt.Select picks from
// them: a pane's listing on the command line. With opt.Totals each directory
// is measured too, in the one walk Z takes in a pane. It returns how many
// operands could not be listed.
func List(ctx context.Context, opt *cli.Options, log *slog.Logger) (int, error) {
	b := newBackend(opt, log)
	var f *nameFilter
	if opt.Select != "" {
		parsed, err := parseSelect(opt.Select)
		if err != nil {
			return 0, err
		}
		f = &parsed
	}
	failed := 0
	for i, raw := range opt.Sources {
		rows, err := b.listOne(ctx, f, raw)
		if err != nil {
			if ctx.Err() != nil {
				return failed, ctx.Err()
			}
			failed++
			if opt.Output == cli.OutputJSON {
				logx.Printf("%s\n", jsonLine(map[string]any{"event": "error", "path": raw, "error": err.Error()}))
			} else {
				logx.Errf("%s: %v\n", cli.Program, err)
			}
			continue
		}
		// Each listing has its own total, as ls gives each directory: one
		// operand may hold another, and a sum over both would count it twice.
		var files, bytes int64
		for _, t := range rows.targets {
			n, size := t.counts()
			files, bytes = files+n, bytes+size
		}
		if opt.Output == cli.OutputJSON {
			for _, t := range rows.targets {
				logx.Printf("%s\n", jsonLine(entryRecord(t)))
			}
			if opt.Totals {
				logx.Printf("%s\n", jsonLine(map[string]any{"event": "total", "path": rows.shown, "files": files, "bytes": bytes}))
			}
			continue
		}
		if len(opt.Sources) > 1 {
			if i > 0 {
				logx.Printf("\n")
			}
			logx.Printf("%s:\n", rows.shown)
		}
		printRows(rows.targets, opt.Totals)
		if opt.Totals {
			logx.Printf("total %s in %s %s\n", humanize.Bytes(bytes), humanize.Count(files), humanize.Plural(files, "file", "files"))
		}
	}
	return failed, nil
}

type listed struct {
	shown   string
	targets []target
}

func (b *backend) listOne(ctx context.Context, f *nameFilter, raw string) (listed, error) {
	if raw == "" {
		return listed{}, errors.New("cannot access '': No such file or directory")
	}
	base, err := parseLocation(raw, b.opt.EndpointSuffix)
	if err != nil {
		return listed{}, err
	}
	shown := raw
	if base.url.IsRemote() {
		shown = base.display()
	}
	n, err := b.store(base.url).Stat(ctx, base.url, true)
	if err != nil {
		return listed{}, &phrased{fmt.Sprintf("cannot access %s: %s", engine.Quote(shown), reason(err)), err}
	}
	if !n.IsDir() {
		// As ls lists a file named on the command line: under its own name.
		e := entry{name: n.Name(), loc: base, node: n}
		if f != nil && !f.selects(e) {
			return listed{shown: shown}, nil
		}
		e.name = shown
		return listed{shown, []target{{e, shown}}}, nil
	}
	entries, err := b.list(ctx, base)
	if err != nil {
		return listed{}, &phrased{fmt.Sprintf("cannot open directory %s: %s", engine.Quote(shown), reason(err)), err}
	}
	if b.opt.Totals || f != nil && f.walks {
		if err := b.measure(ctx, base, entries, f); err != nil {
			return listed{}, &phrased{fmt.Sprintf("cannot measure %s: %s", engine.Quote(shown), reason(err)), err}
		}
	}
	out := listed{shown: shown}
	for _, e := range entries {
		if f == nil || f.selects(e) {
			out.targets = append(out.targets, target{e, shownBelow(raw, base, e)})
		}
	}
	return out, nil
}

// counts is what a row adds to the total: a file itself, or a measured
// directory's contents.
func (t target) counts() (files, bytes int64) {
	switch {
	case t.node.IsRegular():
		return 1, max(t.node.Size, 0)
	case t.dirSize != nil:
		return t.dirSize.files, t.dirSize.bytes
	}
	return 0, 0
}

func kindLetter(n *store.Node) string {
	switch {
	case n.IsDir():
		return "d"
	case n.IsSymlink():
		return "l"
	case n.IsRegular():
		return "-"
	}
	return "?"
}

// when is the time a row shows: a file's own, or with totals the newest file
// inside a directory, since a directory's own time says nothing about them.
func (t target) when() time.Time {
	if t.dirSize != nil {
		return t.dirSize.newest
	}
	return t.node.ModTime
}

func printRows(rows []target, totals bool) {
	type cells struct{ kind, size, files, when, name string }
	var lines []cells
	header := cells{"", "SIZE", "", "MODIFIED", "NAME"}
	if totals {
		header.files, header.when = "FILES", "NEWEST"
	}
	lines = append(lines, header)
	for _, t := range rows {
		c := cells{kind: kindLetter(t.node), size: "-", when: "-", name: lsName(t.name)}
		if t.node.IsDir() && t.name != "/" {
			c.name += "/"
		}
		if t.node.IsRegular() {
			c.size = humanize.Bytes(max(t.node.Size, 0))
		}
		if t.dirSize != nil {
			c.size = humanize.Bytes(t.dirSize.bytes)
		}
		if totals {
			n, _ := t.counts()
			c.files = humanize.Count(n)
		}
		if w := t.when(); !w.IsZero() {
			c.when = w.Local().Format("2006-01-02 15:04")
		}
		lines = append(lines, c)
	}
	var wSize, wFiles, wWhen int
	for _, c := range lines {
		wSize, wFiles, wWhen = max(wSize, len(c.size)), max(wFiles, len(c.files)), max(wWhen, len(c.when))
	}
	for _, c := range lines {
		files := ""
		if totals {
			files = fmt.Sprintf(" %*s", wFiles, c.files)
		}
		kind := c.kind
		if kind == "" {
			kind = " "
		}
		logx.Printf("%s %*s%s  %-*s  %s\n", kind, wSize, c.size, files, wWhen, c.when, c.name)
	}
}

// lsName shows a name as it is unless the shell would need it quoted.
func lsName(name string) string {
	if name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-+,:@%/=") == "" {
		return name
	}
	return engine.Quote(name)
}

func entryRecord(t target) map[string]any {
	kind := map[string]string{"d": "dir", "l": "link", "-": "file", "?": "other"}[kindLetter(t.node)]
	r := map[string]any{"event": "entry", "path": t.shown, "name": t.name, "type": kind}
	if t.node.IsRegular() {
		r["size"] = max(t.node.Size, 0)
	}
	if !t.node.ModTime.IsZero() {
		r["modified"] = t.node.ModTime.UTC().Format(time.RFC3339Nano)
	}
	if t.dirSize != nil {
		r["size"], r["files"] = t.dirSize.bytes, t.dirSize.files
		if !t.dirSize.newest.IsZero() {
			r["newest"] = t.dirSize.newest.UTC().Format(time.RFC3339Nano)
		}
	}
	return r
}
