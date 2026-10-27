// SPDX-License-Identifier: MIT

package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/JohanLindvall/azcp/internal/glob"
)

// nameFilter shows a name when it matches any include term, or when there are
// none, and no exclude term. A term with pattern characters is a bash glob over
// the whole name, case-sensitive as in the shell; any other term is a
// case-insensitive substring, so typing part of a name finds it.
type nameFilter struct {
	text             string
	include, exclude []func(string) bool
}

func parseFilter(text string) (nameFilter, error) {
	f := nameFilter{text: strings.TrimSpace(text)}
	for _, term := range strings.Fields(f.text) {
		list := &f.include
		if rest, ok := strings.CutPrefix(term, "-"); ok && rest != "" {
			list, term = &f.exclude, rest
		}
		m, err := termMatcher(term)
		if err != nil {
			return nameFilter{}, err
		}
		*list = append(*list, m)
	}
	return f, nil
}

func termMatcher(term string) (func(string) bool, error) {
	if !glob.HasMeta(term) {
		term = strings.ToLower(term)
		return func(name string) bool { return strings.Contains(strings.ToLower(name), term) }, nil
	}
	expanded, err := glob.ExpandBraces(term)
	if err != nil {
		return nil, fmt.Errorf("filter %q: %w", term, err)
	}
	var segs []glob.Segment
	for _, alt := range expanded {
		p, err := glob.Compile(alt)
		if err != nil {
			return nil, err
		}
		if len(p.Segs) != 1 || p.Abs || p.TrailingSlash || p.Segs[0].Kind == glob.SegDoubleStar {
			return nil, fmt.Errorf("filter %q: patterns match one name, not a path", term)
		}
		segs = append(segs, p.Segs[0])
	}
	return func(name string) bool {
		return slices.ContainsFunc(segs, func(s glob.Segment) bool { return s.Match(name) })
	}, nil
}

func (f nameFilter) active() bool { return f.text != "" }

func (f nameFilter) match(name string) bool {
	matches := func(m func(string) bool) bool { return m(name) }
	return (len(f.include) == 0 || slices.ContainsFunc(f.include, matches)) &&
		!slices.ContainsFunc(f.exclude, matches)
}

// refilter applies the pane's filter to everything listed. Hidden entries stay
// with the pane, so changing the filter costs no listing, and marks are
// cleared, so nothing out of sight can be copied, moved or deleted.
func (p *pane) refilter() {
	if len(p.entries) == 0 {
		return
	}
	focus := p.cursor - 1
	all := append(slices.Clone(p.entries[1:]), p.hidden...)
	p.entries, p.hidden = p.entries[:1], nil
	p.selected, p.cursor, p.offset = map[int]bool{}, 0, 0
	for i, e := range all {
		if !p.listFilter.match(e.name) {
			p.hidden = append(p.hidden, e)
			continue
		}
		p.entries = append(p.entries, e)
		if i == focus {
			p.cursor = len(p.entries) - 1
		}
	}
	p.sortEntries()
	if p.cursor == 0 && focus >= 0 {
		p.cursor = min(1, len(p.entries)-1)
	}
}

func (a *app) applyFilter(text string) {
	f, err := parseFilter(text)
	if err != nil {
		a.report(err)
		return
	}
	a.modal = nil
	p := &a.panes[a.active]
	if p.usage != nil {
		p.filter = f
		p.usageEntries(nil)
		a.status = "Name filter applied; selection marks cleared"
		return
	}
	p.listFilter = f
	p.refilter()
	a.status = "Filter cleared; selection marks cleared"
	if f.active() {
		a.status = "Filter applied to this pane until cleared; selection marks cleared"
	}
}
