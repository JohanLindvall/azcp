// SPDX-License-Identifier: MIT

package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/uniseg"
	"golang.org/x/term"

	"github.com/JohanLindvall/azcp/internal/cli"
	"github.com/JohanLindvall/azcp/internal/humanize"
	"github.com/JohanLindvall/azcp/internal/logx"
	"github.com/JohanLindvall/azcp/internal/progress"
)

// Run owns the terminal until all of its workers have stopped. Merely omitting
// cp operands never starts it; main calls it only for --tui.
func Run(ctx context.Context, opt *cli.Options, logger *slog.Logger) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return errors.New("--tui requires a terminal on stdin and stderr")
	}
	b := newBackend(opt, logger)
	s, err := tcell.NewScreen()
	if err != nil {
		return fmt.Errorf("open terminal: %w", err)
	}
	if err := s.Init(); err != nil {
		return fmt.Errorf("initialize terminal: %w", err)
	}
	defer logx.WithTerminal(s.Fini)
	a, err := newApp(ctx, s, b)
	if err != nil {
		return err
	}
	return a.run()
}

type pane struct {
	loc         location
	entries     []entry // entry zero is always the parent
	cursor      int
	offset      int
	selected    map[int]bool
	sort        paneSort
	showCounts  bool
	usage       *usageTree
	usageDir    *usageNode
	filter      nameFilter // the usage view's, cleared on leaving a directory
	listFilter  nameFilter // the two-pane view's, kept across navigation
	hidden      []entry    // listed entries listFilter rules out
	savedSort   paneSort
	savedCounts bool
}

func (p *pane) selection() []entry {
	var out []entry
	for i, e := range p.entries {
		if i > 0 && p.selected[i] {
			out = append(out, e)
		}
	}
	if len(out) == 0 && p.cursor > 0 && p.cursor < len(p.entries) {
		out = append(out, p.entries[p.cursor])
	}
	return out
}

type jobResult struct {
	value any
	err   error
}

type job struct {
	label    string
	cancel   context.CancelFunc
	done     chan jobResult
	finish   func(jobResult)
	progress *progress.Reporter
	sizing   bool
}

type dialog struct {
	kind   string
	title  string
	text   string
	lines  []string
	scroll int
	action string
	fresh  bool // the text was offered, not typed: typing replaces it
	source []entry
	target location
}

type messages struct {
	mu    sync.Mutex
	lines []string
}

func (m *messages) add(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(logx.Redact(s)), "\n") {
		if line != "" {
			m.lines = append(m.lines, line)
		}
	}
	if len(m.lines) > 100 {
		m.lines = append([]string(nil), m.lines[len(m.lines)-100:]...)
	}
}

func (m *messages) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.lines...)
}

type app struct {
	ctx     context.Context
	screen  tcell.Screen
	theme   theme
	backend *backend
	panes   [2]pane
	active  int
	busy    *job
	modal   *dialog
	status  string
	logs    messages
	sizeGen uint64 // advanced by an explicit refresh; older calculated totals are stale
	workers sync.WaitGroup
	quit    bool
	pasting bool
	single  bool // one pane: browsing and deleting, never copying between panes
}

func newApp(ctx context.Context, s tcell.Screen, b *backend) (*app, error) {
	a := &app{ctx: ctx, screen: s, theme: newTheme(s.Colors()), backend: b, status: "Ready",
		single: b.opt.TUIMode == cli.TUISingle}
	for i, raw := range b.opt.TUIPaths {
		loc, err := parseLocation(raw, b.opt.EndpointSuffix)
		if err != nil {
			return nil, err
		}
		a.panes[i] = pane{loc: loc, selected: map[int]bool{}}
	}
	return a, nil
}

func (a *app) run() error {
	ctx, cancel := context.WithCancel(a.ctx)
	a.ctx = ctx
	logx.SetTerminalSink(a.logs.add)
	defer func() {
		cancel()
		a.workers.Wait()
		logx.SetTerminalSink(nil)
	}()
	events := make(chan tcell.Event, 16)
	stopEvents, eventsStopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(eventsStopped)
		a.screen.ChannelEvents(events, stopEvents)
	}()
	defer func() { close(stopEvents); <-eventsStopped }()
	a.screen.EnablePaste()
	ticker := time.NewTicker(progress.DefaultInterval)
	defer ticker.Stop()
	if a.backend.opt.TUIMode == cli.TUIUsage {
		a.scanUsagePane(0, a.panes[0].loc, "", false)
	} else {
		a.reload()
	}
	for {
		a.draw()
		var done <-chan jobResult
		var ticks <-chan time.Time
		if a.busy != nil {
			done = a.busy.done
			ticks = ticker.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-done:
			j := a.busy
			j.cancel()
			a.busy = nil
			if r.err != nil {
				a.report(r.err)
			}
			if a.quit {
				return nil
			}
			j.finish(r)
		case event, ok := <-events:
			if !ok {
				return nil
			}
			switch ev := event.(type) {
			case *tcell.EventKey:
				a.key(ev)
			case *tcell.EventResize:
				logx.WithTerminal(a.screen.Sync)
			case *tcell.EventPaste:
				a.pasting = ev.Start()
			}
		case <-ticks:
		}
		if a.quit && a.busy == nil {
			return nil
		}
	}
}

func (a *app) start(label string, work func(context.Context, *progress.Reporter) (any, error), finish func(jobResult)) {
	ctx, cancel := context.WithCancel(a.ctx)
	j := &job{label: label, cancel: cancel, done: make(chan jobResult, 1), finish: finish,
		progress: progress.New(progress.Config{Mode: progress.ModeNever})}
	a.busy = j
	a.workers.Go(func() {
		defer j.progress.Stop()
		value, err := work(ctx, j.progress)
		j.done <- jobResult{value, err}
	})
}

func (a *app) report(err error) {
	if errors.Is(err, context.Canceled) {
		a.status = "Cancelled; completed changes are not undone"
	} else {
		a.status = "Error: " + logx.Redact(err.Error())
	}
	a.logs.add(a.status)
}

func (a *app) setEntries(i int, loc location, entries []entry) {
	p := &a.panes[i]
	parent := parentLocation(loc)
	if loc.sizes == nil || loc.sizes.gen != a.sizeGen {
		loc.sizes = &sizeTree{gen: a.sizeGen}
	}
	// Children must return to this tree, not the listing's copy of the
	// location, or totals calculated here are forgotten on the way back up.
	here := loc
	for i := range entries {
		e := &entries[i]
		e.loc.parent = &here
		if child := loc.sizes.children[e.name]; child != nil && e.node != nil && e.node.IsDir() {
			child.gen = loc.sizes.gen
			e.loc.sizes = child
			if !child.stale {
				e.dirSize = child
			}
		}
	}
	p.loc = loc
	p.entries, p.hidden = append([]entry{{name: "..", loc: parent}}, entries...), nil
	// Marks belong to the previous listing's rows, not this one's.
	p.selected, p.cursor = map[int]bool{}, 0
	p.refilter()
	p.cursor = min(1, len(p.entries)-1)
}

func (a *app) reload() {
	locations := [2]location{a.panes[0].loc, a.panes[1].loc}
	type listing struct {
		entries []entry
		usage   *usageTree
		err     error
	}
	usage := [2]*usageTree{a.panes[0].usage, a.panes[1].usage}
	current := [2]string{a.panes[0].usagePath(), a.panes[1].usagePath()}
	single := a.single
	a.start("Refreshing panes", func(ctx context.Context, progress *progress.Reporter) (any, error) {
		var result [2]listing
		for i, loc := range locations {
			if single && i == 1 {
				break
			}
			if usage[i] != nil {
				result[i].usage, result[i].err = a.backend.scanUsage(ctx, usage[i].base, usage[i].oneFS, progress)
			} else {
				result[i].entries, result[i].err = a.backend.list(ctx, loc)
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return result, nil
	}, func(r jobResult) {
		if r.err != nil {
			return
		}
		for i, l := range r.value.([2]listing) {
			if single && i == 1 {
				break
			}
			if l.err != nil {
				a.report(l.err)
				// A failed refresh cannot leave stale entries actionable.
				a.panes[i].leaveUsage()
				a.setEntries(i, locations[i], nil)
			} else if l.usage != nil {
				a.setUsage(i, l.usage, current[i])
			} else {
				a.setEntries(i, locations[i], l.entries)
			}
		}
	})
}

func (a *app) navigate(loc location) {
	i := a.active
	if p := &a.panes[i]; p.usage != nil {
		a.scanUsagePane(i, loc, "", p.usage.oneFS)
		return
	}
	a.start("Reading "+loc.display(), func(ctx context.Context, _ *progress.Reporter) (any, error) {
		return a.backend.list(ctx, loc)
	}, func(r jobResult) {
		if r.err == nil {
			a.setEntries(i, loc, r.value.([]entry))
		}
	})
}

func (a *app) key(ev *tcell.EventKey) {
	// A pasted newline or shortcut must never confirm a destructive action.
	// Text is accepted only inside the path/name prompt, without submission.
	if a.pasting {
		if a.modal != nil && a.modal.kind == "input" && ev.Key() == tcell.KeyRune && ev.Rune() >= ' ' && len(a.modal.text) < 8192 {
			if a.modal.fresh {
				a.modal.text, a.modal.fresh = "", false
			}
			a.modal.text += string(ev.Rune())
		}
		return
	}
	if a.modal != nil {
		a.dialogKey(ev)
		return
	}
	k, r := ev.Key(), ev.Rune()
	if k == tcell.KeyF9 || r == 'l' {
		a.modal = &dialog{kind: "view", title: "Messages (latest 100)", lines: a.logs.snapshot()}
		return
	}
	if a.busy != nil {
		if k == tcell.KeyEscape || k == tcell.KeyCtrlC || k == tcell.KeyF10 || r == 'q' {
			a.busy.cancel()
			a.busy.label = "Cancelling; waiting for active work to stop"
			a.quit = k == tcell.KeyF10 || r == 'q'
		}
		return
	}
	p := &a.panes[a.active]
	_, height := a.screen.Size()
	page := paneRows(height)
	switch {
	case k == tcell.KeyF10 || k == tcell.KeyCtrlC || r == 'q':
		a.quit = true
	case (k == tcell.KeyTab || k == tcell.KeyBacktab) && a.single:
		a.status = "Single-pane mode has no second pane"
	case k == tcell.KeyTab || k == tcell.KeyBacktab:
		a.active = 1 - a.active
		if a.panes[a.active].entries == nil {
			a.navigate(a.panes[a.active].loc)
		}
	case k == tcell.KeyUp || r == 'k':
		p.cursor = max(0, p.cursor-1)
	case k == tcell.KeyDown || r == 'j':
		p.cursor = min(len(p.entries)-1, p.cursor+1)
	case k == tcell.KeyPgUp:
		p.cursor = max(0, p.cursor-page)
	case k == tcell.KeyPgDn:
		p.cursor = min(len(p.entries)-1, p.cursor+page)
	case k == tcell.KeyHome:
		p.cursor = 0
	case k == tcell.KeyEnd:
		p.cursor = len(p.entries) - 1
	case k == tcell.KeyInsert || r == ' ':
		if p.cursor > 0 && p.cursor < len(p.entries) {
			p.selected[p.cursor] = !p.selected[p.cursor]
			p.cursor = min(len(p.entries)-1, p.cursor+1)
		}
	case r == '*':
		for i := 1; i < len(p.entries); i++ {
			p.selected[i] = !p.selected[i]
		}
	case k == tcell.KeyBackspace || k == tcell.KeyBackspace2 || k == tcell.KeyLeft:
		if p.usage != nil {
			a.usageParent()
		} else {
			a.navigate(parentLocation(p.loc))
		}
	case k == tcell.KeyEnter || k == tcell.KeyRight:
		if p.cursor >= 0 && p.cursor < len(p.entries) {
			e := p.entries[p.cursor]
			if p.usage != nil && p.cursor == 0 {
				a.usageParent()
			} else if p.usage != nil && e.directory() {
				a.openUsage(e.usage)
			} else if e.directory() {
				a.navigate(e.loc)
			} else {
				a.preview(e)
			}
		}
	case k == tcell.KeyF1 || r == '?':
		a.modal = &dialog{kind: "view", title: "azcp Commander", lines: strings.Split(help, "\n")}
	case k == tcell.KeyF2 || r == 'v':
		a.navigate(rootLocation(p.loc))
	case k == tcell.KeyF3:
		if p.cursor > 0 && p.cursor < len(p.entries) {
			a.preview(p.entries[p.cursor])
		}
	case k == tcell.KeyF4 || r == 'g':
		a.modal = &dialog{kind: "input", title: "Go to local directory or Azure URL", action: "go", target: p.loc}
	case k == tcell.KeyF5 || r == 'c':
		a.requestOperation("Copy")
	case k == tcell.KeyF6 || r == 'm':
		a.requestOperation("Move")
	case k == tcell.KeyF7 || r == 'n':
		a.modal = &dialog{kind: "input", title: "Create directory (one name)", action: "mkdir", target: p.loc}
	case k == tcell.KeyF8 || k == tcell.KeyDelete || r == 'd':
		a.requestOperation("Delete")
	case k == tcell.KeyCtrlR || r == 'r':
		if p.usage != nil {
			a.scanUsagePane(a.active, p.usage.base, p.usagePath(), p.usage.oneFS)
		} else {
			a.sizeGen++
			a.reload()
		}
	case r == 's':
		a.modal = &dialog{kind: "sort", title: "Sort active pane"}
	case r == 'f':
		a.toggleCounts()
	case r == 'z' || r == 'Z':
		if p.usage != nil {
			a.scanUsagePane(a.active, p.usage.base, p.usagePath(), p.usage.oneFS)
		} else {
			a.calculateSizes(r == 'Z')
		}
	case r == 'u':
		a.toggleUsage()
	case r == 'a' && p.usage != nil:
		if p.loc.url.IsRemote() {
			a.status = "Azure reports stored blob lengths; physical disk allocation is unavailable"
		} else {
			p.sort.allocated = !p.sort.allocated
			if p.filter.walks {
				// The filter weighs the column shown, which has just changed.
				var focus *usageNode
				if p.cursor > 0 {
					focus = p.entries[p.cursor].usage
				}
				p.usageEntries(focus)
			} else {
				p.sortEntries()
			}
		}
	case r == 'x' && p.usage != nil:
		if p.loc.url.IsRemote() {
			a.status = "Filesystem boundaries apply to local scans"
		} else {
			a.scanUsagePane(a.active, p.usage.base, p.usagePath(), !p.usage.oneFS)
		}
	case r == '/':
		text := p.listFilter.text
		if p.usage != nil {
			text = p.filter.text
		}
		a.modal = &dialog{kind: "input", title: "Filter this pane", action: "filter", text: text, fresh: text != ""}
	case r == 'i' && p.usage != nil:
		a.usageInfo()
	}
}

func (a *app) requestOperation(action string) {
	if a.single && action != "Delete" {
		a.status = action + " needs a destination pane; single-pane mode only browses and deletes"
		return
	}
	p := &a.panes[a.active]
	selected := p.selection()
	if p.usage != nil {
		for i, e := range selected {
			selected[i] = p.usage.entry(e.usage)
		}
	}
	if len(selected) == 0 {
		a.status = "Select an entry first"
		return
	}
	for _, e := range selected {
		if action == "Delete" || action == "Move" {
			if e.usage != nil && (e.usage.total.incomplete || e.usage.total.excluded > 0) {
				a.report(errors.New("selection contains unreadable or excluded entries; refresh a complete scan before moving or deleting it"))
				return
			}
			if err := removable(e); err != nil {
				a.report(err)
				return
			}
		}
	}
	d := &dialog{kind: "confirm", title: action + " selected entries", action: action,
		source: selected, target: a.panes[1-a.active].loc}
	if action == "Delete" {
		d.title = "Delete permanently"
	}
	a.modal = d
}

func (a *app) dialogKey(ev *tcell.EventKey) {
	d := a.modal
	k, r := ev.Key(), ev.Rune()
	if k == tcell.KeyEscape || k == tcell.KeyCtrlC {
		a.modal = nil
		if k == tcell.KeyCtrlC && a.busy != nil {
			a.busy.cancel()
		}
		return
	}
	if d.kind == "sort" {
		a.sortKey(ev)
		return
	}
	if d.kind == "view" {
		switch k {
		case tcell.KeyUp:
			d.scroll = max(0, d.scroll-1)
		case tcell.KeyDown:
			d.scroll++
		case tcell.KeyPgUp:
			d.scroll = max(0, d.scroll-10)
		case tcell.KeyPgDn:
			d.scroll += 10
		case tcell.KeyHome:
			d.scroll = 0
		case tcell.KeyEnd:
			d.scroll = len(d.lines)
		case tcell.KeyEnter, tcell.KeyF3, tcell.KeyF9:
			a.modal = nil
		}
		if r == 'q' {
			a.modal = nil
		}
		return
	}
	if d.kind == "confirm" {
		if r == 'y' || r == 'Y' {
			a.modal = nil
			a.execute(d)
		} else if r == 'n' || r == 'N' || k == tcell.KeyEnter {
			a.modal = nil
		} else if (r == 'e' || r == 'E') && (d.action == "Copy" || d.action == "Move") {
			d.kind, d.title = "input", d.action+" to directory"
		}
		return
	}
	fresh := d.fresh
	d.fresh = false
	switch k {
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		g := uniseg.NewGraphemes(d.text)
		last := 0
		for g.Next() {
			last, _ = g.Positions()
		}
		d.text = d.text[:last]
	case tcell.KeyCtrlU:
		d.text = ""
	case tcell.KeyRune:
		if fresh {
			d.text = ""
		}
		if len(d.text) < 8192 && r >= ' ' {
			d.text += string(r)
		}
	case tcell.KeyEnter:
		if d.action == "filter" {
			a.applyFilter(d.text)
			return
		}
		if d.action == "mkdir" {
			a.modal = nil
			a.execute(d)
			return
		}
		loc := d.target
		if d.text != "" {
			base := d.target
			if d.action == "Copy" || d.action == "Move" {
				// Reopening the editor must not resolve a relative path
				// against the destination it previously produced.
				base = a.panes[1-a.active].loc
			}
			var err error
			loc, err = a.backend.resolve(d.text, base)
			if err != nil {
				a.report(err)
				return
			}
		}
		if d.action == "go" {
			a.modal = nil
			a.navigate(loc)
			return
		}
		d.target, d.kind = loc, "confirm"
		d.title = d.action + " selected entries"
	}
}

func (a *app) execute(d *dialog) {
	type result struct {
		text  string
		usage *usageTree
	}
	i := a.active
	usage, current := a.panes[i].usage, a.panes[i].usagePath()
	a.start(d.action, func(ctx context.Context, p *progress.Reporter) (any, error) {
		var err error
		switch d.action {
		case "Copy", "Move":
			err = a.backend.transfer(ctx, d.source, d.target, d.action == "Move", p)
		case "Delete":
			err = a.backend.delete(ctx, d.source)
		case "mkdir":
			err = a.backend.mkdir(ctx, d.target, d.text)
		}
		done, _, _, _, bytes, _ := p.Totals()
		out := result{text: fmt.Sprintf("%s completed (%d files, %s copied)", d.action, done, humanize.Bytes(bytes))}
		if err == nil && d.action == "Delete" && usage != nil && !a.backend.opt.DryRun {
			out.usage, err = a.backend.usageAfterDelete(ctx, usage, d.source)
			out.text = "Deletion completed; cached usage totals updated"
		}
		return out, err
	}, func(r jobResult) {
		if !a.backend.opt.DryRun {
			// Forgotten even after a failure, which can leave partial changes.
			a.forgetSizes(d.changes())
		}
		if r.err == nil {
			out := r.value.(result)
			if out.usage != nil {
				a.setUsage(i, out.usage, current)
				a.status = out.text
				if !a.single {
					a.refreshOtherPane(1 - i)
				}
				return
			}
			a.status = out.text
			if a.backend.opt.DryRun {
				a.status = "Dry run: " + d.action + " planned; no changes made"
				if usage != nil {
					return
				}
			}
		} else if usage != nil {
			// A failed/cancelled mutation can have made partial changes. Drop
			// cached totals and load live listings without launching a new scan.
			a.panes[0].leaveUsage()
			a.panes[1].leaveUsage()
		}
		a.reload()
	})
}

func (a *app) preview(e entry) {
	if e.usage != nil {
		e = a.panes[a.active].usage.entry(e.usage)
	}
	a.start("Reading preview", func(ctx context.Context, _ *progress.Reporter) (any, error) {
		return a.backend.preview(ctx, e)
	}, func(r jobResult) {
		if r.err == nil {
			a.modal = &dialog{kind: "view", title: e.loc.display(), lines: strings.Split(r.value.(string), "\n")}
		}
	})
}

const help = `Two panes browse local folders, Azure accounts, containers and prefixes;
--tui=single shows one, for browsing and deleting only. Tab switches panes.
Arrows, Home/End and Page Up/Down navigate.
Enter opens a folder or previews a file. Backspace goes to the parent.
Space / Insert marks entries; * inverts marks. No marks means the cursor.

F2 / v   Root of the current filesystem or Azure container
F3       Read-only text preview (first 64 KiB)
F4 / g   Go to a local path or Azure URL
F5 / c   Copy to the other pane or an entered directory (two panes only)
F6 / m   Move: copy, then remove sources only after full success
F7 / n   Create directory (Azure container needs --create-container)
F8 / d   Delete selected entries and their contents permanently
F9 / l   Messages, including Azure sign-in instructions
F10 / q  Quit (cancel active work and wait for it to stop)
Ctrl-R/r Refresh both panes
s        Sort this pane: name, modified time, size, extension or type
         Also recursive file count (6) and newest file inside (7)
f        Toggle this pane's size / recursive file-count column
z / Z    Calculate directory totals: marked/cursor / all in this pane
/        Filter this pane until cleared; the prompt lists the syntax
         report *.mp4   names (any may match; other terms must all hold)
         size>50M files>=100 newer:7d older:2024-01-01 type:dir
         has:.mp4  directories holding a matching file; -term excludes
         Directory sizes, ages and contents are measured when asked for
u        Scan a cached disk-usage view / return to two panes

Disk-usage view (also available with --tui=usage):
Enter / Backspace navigate the scanned tree without listing it again.
a toggles disk allocation / logical size; Azure shows stored blob lengths.
/ filters names in this directory; i shows details; x toggles crossing filesystem boundaries.
r refreshes the scan. F8 / d deletes with confirmation.
H marks hard links, ! incomplete totals, > excluded directories.
Incomplete or excluded trees cannot be moved or deleted from this view.

Copy and move use the other pane; e edits the destination in confirmation.
Copy, move and delete require y in the confirmation dialog.
Existing destination files are replaced. Enter/Esc cancels confirmation.
Esc/Ctrl-C cancels active work. Completed changes are not rolled back.
Azure account/container roots cannot be removed; select their contents.
Ctrl-U clears a path prompt. Relative destinations use the other pane.
The sort menu reverses order and toggles directories first. Marks stay put.
Directory sizes and file counts are calculated together; links are skipped.
Unknown totals sort last within their group. Refresh clears calculated totals.
Only --tui starts this interface; ordinary invocations remain cp.`
