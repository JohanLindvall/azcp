// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/engine"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/uri"
)

// The command line reaches the same selections as the file manager: --select
// is the pane filter applied to each operand directory, and --remove is the
// delete key. Both reuse what the panes do, so a filter means one thing and a
// deletion keeps the same guarantees wherever it was asked for.

// Select replaces each source directory in opt with the entries of it that
// opt.Select picks, written as literal operands for the engine.
func Select(ctx context.Context, opt *cli.Options, log *slog.Logger) error {
	b := newBackend(opt, log)
	picked, err := b.selectEach(ctx, opt.Select, opt.Sources)
	if err != nil {
		return err
	}
	opt.Sources = opt.Sources[:0]
	for _, t := range picked {
		e := t.entry
		opt.Sources = append(opt.Sources, operand(e.loc.url, e.node.IsDir()))
	}
	// Selected names are exact; a bracket in one is not a pattern.
	opt.Glob = cli.GlobNever
	return nil
}

// target is an entry with the name messages give it, which starts from the
// operand as it was typed, the way cp's own messages do.
type target struct {
	entry
	shown string
}

func shownBelow(raw string, base location, e entry) string {
	if base.url.IsRemote() {
		return e.loc.display()
	}
	return filepath.Join(raw, e.name)
}

// selectEach lists every directory and keeps what the filter selects. Only an
// entry the filter can positively judge is selected: unlike a pane, which keeps
// an unmeasured directory on show, a batch acts on what it picks without
// anyone looking first.
func (b *backend) selectEach(ctx context.Context, text string, dirs []string) ([]target, error) {
	f, err := parseFilter(text)
	if err != nil {
		return nil, cli.Usage(fmt.Errorf("--select: %w", err))
	}
	var picked []target
	for _, raw := range dirs {
		base, err := parseLocation(raw, b.opt.EndpointSuffix)
		if err != nil {
			return nil, err
		}
		shown := engine.Quote(base.display())
		if !base.url.IsRemote() {
			shown = engine.Quote(raw)
		}
		n, err := b.store(base.url).Stat(ctx, base.url, true)
		if err != nil {
			return nil, fmt.Errorf("cannot stat %s: %w", shown, describe(err))
		}
		if !n.IsDir() {
			return nil, fmt.Errorf("cannot select from %s: Not a directory", shown)
		}
		entries, err := b.list(ctx, base)
		if err != nil {
			return nil, fmt.Errorf("cannot list %s: %w", shown, describe(err))
		}
		if err := b.measureFor(ctx, f, base, entries); err != nil {
			return nil, err
		}
		for _, e := range entries {
			if f.selects(e) {
				picked = append(picked, target{e, shownBelow(raw, base, e)})
			}
		}
	}
	return picked, nil
}

// measureFor walks the directories among entries once, together, when the
// filter asks about sizes, ages or contents: the walk Z takes in a pane.
func (b *backend) measureFor(ctx context.Context, f nameFilter, base location, entries []entry) error {
	if !f.walks {
		return nil
	}
	var dirs []int
	for i, e := range entries {
		if e.node.IsDir() {
			dirs = append(dirs, i)
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	selected := make([]entry, len(dirs))
	for i, row := range dirs {
		selected[i] = entries[row]
	}
	quiet := progress.New(progress.Config{Mode: progress.ModeNever})
	defer quiet.Stop()
	trees, err := b.directorySizes(ctx, base, selected, true, f.has, quiet)
	if err != nil {
		return err
	}
	for i, row := range dirs {
		entries[row].dirSize = trees[i]
	}
	return nil
}

// selects is match without the benefit of the doubt: a term that cannot judge
// an entry neither selects it nor fails to exclude it.
func (f nameFilter) selects(e entry) bool {
	holds := func(t filterTerm) bool {
		match, known := t(e, false)
		return match && known
	}
	if len(f.names) > 0 && !slices.ContainsFunc(f.names, holds) {
		return false
	}
	for _, t := range f.must {
		if !holds(t) {
			return false
		}
	}
	return !slices.ContainsFunc(f.excluded, func(t filterTerm) bool {
		match, known := t(e, false)
		return match || !known
	})
}

// Remove deletes the operands, or what opt.Select picks from them, and lists
// them instead unless opt.Confirmed. Every selection is listed in full before
// anything is removed, as the file manager's delete does. It returns how many
// operands failed.
func Remove(ctx context.Context, opt *cli.Options, log *slog.Logger) (int, error) {
	b := newBackend(opt, log)
	var targets []target
	if opt.Select != "" {
		picked, err := b.selectEach(ctx, opt.Select, opt.Sources)
		if err != nil {
			return 0, err
		}
		targets = picked
	} else {
		for _, raw := range opt.Sources {
			l, err := parseLocation(raw, b.opt.EndpointSuffix)
			if err != nil {
				return 0, err
			}
			shown := raw
			if l.url.IsRemote() {
				shown = l.display()
			}
			n, err := b.store(l.url).Stat(ctx, l.url, false)
			if err != nil {
				return 0, fmt.Errorf("cannot remove %s: %w", engine.Quote(shown), describe(err))
			}
			targets = append(targets, target{entry{name: n.Name(), loc: l, node: n}, shown})
		}
	}
	failed := 0
	kept := targets[:0]
	var entries []entry
	for _, e := range targets {
		err := removable(e.entry)
		if err == nil && e.node.IsDir() && !opt.Recursive {
			err = errors.New("Is a directory")
		}
		if err != nil {
			logx.Errf("%s: cannot remove %s: %v\n", cli.Program, engine.Quote(e.shown), err)
			failed++
			continue
		}
		kept = append(kept, e)
		entries = append(entries, e.entry)
	}
	targets = kept
	trees, err := b.inventory(ctx, entries)
	if err != nil {
		return failed, err
	}
	dryRun := opt.DryRun || !opt.Confirmed
	var files, bytes int64
	removed, uncounted := 0, false
	for i, tree := range trees {
		var n, size int64
		count := func(node *store.Node) error {
			if node.IsRegular() {
				n++
				size += max(node.Size, 0)
			}
			return ctx.Err()
		}
		for _, node := range tree {
			_ = count(node)
		}
		uncounted = uncounted || !dryRun && isContainer(tree[0].URL)
		if dryRun && isContainer(tree[0].URL) {
			// The inventory leaves a container unlisted because one request
			// deletes it; a listing of what would go needs its contents.
			err := b.remote.WalkAll(ctx, tree[0].URL, func(_ *uri.URL, err error) error { return err }, count)
			if err != nil {
				return failed, fmt.Errorf("cannot list %s: %w", engine.Quote(targets[i].shown), err)
			}
		}
		if !dryRun {
			if err := b.remove(ctx, trees[i:i+1]); err != nil {
				if ctx.Err() != nil {
					return failed, ctx.Err()
				}
				logx.Errf("%s: %v\n", cli.Program, describe(err))
				failed++
				continue
			}
		}
		files, bytes, removed = files+n, bytes+size, removed+1
		if dryRun || opt.Verbose || opt.Output == cli.OutputJSON {
			report(opt, dryRun, targets[i], n, size)
		}
	}
	if opt.Output != cli.OutputJSON {
		verb := "Removed"
		if dryRun {
			verb = "Would remove"
		}
		totals := fmt.Sprintf(": %s in %s %s", humanize.Bytes(bytes), humanize.Count(files), humanize.Plural(files, "file", "files"))
		if uncounted {
			totals = "" // A container went in one request, its contents uncounted.
		}
		logx.Errf("  %s %d %s%s\n", verb, removed, humanize.Plural(int64(removed), "entry", "entries"), totals)
		if dryRun && !opt.DryRun && removed > 0 {
			logx.Errf("  Nothing was removed; add --yes to remove them\n")
		}
	}
	return failed, nil
}

func report(opt *cli.Options, dryRun bool, e target, files, bytes int64) {
	if opt.Output == cli.OutputJSON {
		kind := "removed"
		if dryRun {
			kind = "would-remove"
		}
		line, err := json.Marshal(map[string]any{"event": kind, "path": e.shown,
			"directory": e.node.IsDir(), "files": files, "bytes": bytes})
		if err != nil {
			line = []byte(`{"event":"error","error":"could not encode this record"}`)
		}
		logx.Printf("%s\n", line)
		return
	}
	name := engine.Quote(e.shown)
	switch {
	case dryRun && e.node.IsDir():
		logx.Printf("would remove %s (%s in %s %s)\n", name, humanize.Bytes(bytes),
			humanize.Count(files), humanize.Plural(files, "file", "files"))
	case dryRun:
		logx.Printf("would remove %s (%s)\n", name, humanize.Bytes(bytes))
	case isContainer(e.loc.url):
		logx.Printf("removed container %s\n", name)
	case e.node.IsDir():
		logx.Printf("removed directory %s\n", name)
	default:
		logx.Printf("removed %s\n", name)
	}
}

// describe gives a missing name the wording cp uses for one.
func describe(err error) error {
	if store.IsNotExist(err) {
		return errors.New("No such file or directory")
	}
	return err
}
