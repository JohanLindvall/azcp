package engine

import (
	"github.com/JohanLindvall/azcp/internal/store"
	"github.com/JohanLindvall/azcp/internal/store/local"
	"github.com/JohanLindvall/azcp/internal/uri"
)

func (e *Engine) hardLinkID(src *store.Node, dst *uri.URL) (local.FileID, bool) {
	if e.opt.HardLink || e.opt.SymbolicLink || src.URL.IsRemote() || dst.IsRemote() {
		return local.FileID{}, false
	}
	info, err := sourceAttrs(src)
	if err != nil {
		return local.FileID{}, false
	}
	id, count, ok := local.IDOf(src.URL.Path, info)
	return id, ok && count > 1
}

// Claims belong to operand order, not worker scheduling: -u may retain the
// first destination and later names must link to that same file.
func (e *Engine) planHardLink(t *task) {
	if !e.opt.Preserve.Links || t.hardLink != nil {
		return
	}
	id, ok := e.hardLinkID(t.src, t.dst)
	if !ok {
		return
	}
	e.hardLinksMu.Lock()
	defer e.hardLinksMu.Unlock()
	t.hardLink = e.hardLinks[id]
	if t.hardLink == nil {
		t.hardLink = &linkFuture{done: make(chan struct{})}
		t.linkClaim = &linkClaim{id: id, fut: t.hardLink}
		e.hardLinks[id] = t.hardLink
	}
}

func (e *Engine) priorHardLink(src *store.Node, dst *uri.URL) *linkFuture {
	if !e.opt.Preserve.Links {
		return nil
	}
	id, ok := e.hardLinkID(src, dst)
	if !ok {
		return nil
	}
	e.hardLinksMu.Lock()
	defer e.hardLinksMu.Unlock()
	return e.hardLinks[id]
}

func (e *Engine) keepHardLink(src *store.Node, dst *uri.URL) *linkFuture {
	id, ok := e.hardLinkID(src, dst)
	if !ok {
		return nil
	}
	e.hardLinksMu.Lock()
	defer e.hardLinksMu.Unlock()
	if prior := e.hardLinks[id]; prior != nil {
		return prior
	}
	// GNU cp also links two destinations both skipped by -u, even without
	// --preserve=links. Only retained destinations enter this map in that case.
	fut := &linkFuture{done: make(chan struct{}), path: dst.Path, ok: true}
	close(fut.done)
	e.hardLinks[id] = fut
	return nil
}
