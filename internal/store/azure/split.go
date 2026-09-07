package azure

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/JohanLindvall/azcp/internal/store"
)

// A flat listing is one request per five thousand blobs, and its pages are
// strictly sequential: the marker that fetches the next arrives at the end of
// the last. A container of a quarter of a million blobs is therefore fifty
// round trips in single file, four megabytes apiece, with nothing else
// happening in between — and every transfer in the run waiting on it.
//
// The link is rarely what makes that slow. Measured against a real account,
// forty-four megabytes of listing took 30.0s fetched one request at a time and
// 3.9s with sixteen in flight, and that account's whole rerun spent 129 of its
// 154 seconds inside a single container. What is missing is not bandwidth but
// depth.
//
// So a listing that has been going long enough, with more still to come,
// divides. One request asks what lies immediately beneath the prefix, those
// names are cut into a few key ranges that between them cover everything under
// it, and the ranges are listed at the same time. The caller still sees one
// container after another and every blob's directories before the blob; what it
// no longer sees is the container's own names in sorted order, which
// `Store.WalkAll` never promised and nothing downstream reads.
//
// It buys the depth with requests — ranges do not divide evenly, so each ends
// on a partial page, and finding them costs a listing of its own. Against fifty
// slow pages that is noise: a rerun of 258,000 blobs took 72 listings where an
// undivided one took about 55, and 45 seconds where it took 154. Against a
// container that answers in one page, or in a few fast ones, it would be the
// whole cost, which is why the question is not asked until the listing has
// already spent longer than the ranges could: the account of ten thousand small
// containers pays nothing for this.
//
// Two obvious refinements were tried against those accounts and are slower.
// Waiting for a second page before asking spares the two requests a container
// that ends on page two spends for nothing — but costs three seconds on the one
// that matters, and only moves the same waste onto three-page containers.
// Asking while the listing carries on, so the round trip costs no wall time,
// costs seven: the question comes back in under a second and a page takes two
// and a half, so overlapping them buys a cheap answer at the price of an
// expensive page fetched in order. Ask, and wait for the answer.

const (
	// maxSplitWays is how many divided listings the whole walk runs at once.
	// Each holds the page it has fetched and the page it is handing over, so
	// this is what the extra memory is proportional to — a page of five
	// thousand parsed blobs is a few megabytes.
	maxSplitWays = 8
	// maxSplitDepth bounds how many levels of prefix are asked about while
	// looking for enough ranges. Each level costs a listing per prefix opened
	// up, and a tree that has not fanned out within three of them is one where
	// the names, not the shape, will have to do the dividing.
	maxSplitDepth = 3
	// maxSplitUnits caps what one level may be expanded into, so a prefix
	// holding a hundred thousand subdirectories cannot turn a listing into a
	// hundred thousand of them.
	maxSplitUnits = 4096
	// splitAfterTime is how long a listing may take before it is worth asking
	// whether it can be divided. Dividing costs a request to find the ranges
	// and one more per range, since none of them ends on a page boundary, so
	// it has to be paid for out of the round trips it saves — and a round trip
	// is only worth saving where it costs something. Against a real account a
	// page took 2.5 seconds and the question was worth asking after the first;
	// against the emulator, where a page comes back in fifty milliseconds, a
	// container is over before the second is due.
	splitAfterTime = time.Second
	// splitAfterPages is the backstop for an endpoint fast enough that the
	// clock never trips. Eight pages with more still to come is forty thousand
	// blobs, and however cheap the round trips are there are a great many of
	// them left.
	splitAfterPages = 8
	// maxSplitRanges is the ceiling on the ranges. A group divides
	// into one range per distinct byte at the point its names diverge, all or
	// none — there is no prefix that covers some of them and not the others —
	// so the count is whatever the names say, and each range ends on a partial
	// page it has to pay a request for. Names that fan out further than this
	// are left to page through in order, which costs what it always did.
	// Hexadecimal, digits and the lower-case alphabet all fit.
	maxSplitRanges = 32
)

// blobPager is what a flat listing is read through.
type blobPager = azruntime.Pager[container.ListBlobsFlatResponse]

// keyRange is one part of a divided listing: everything under prefix that
// sorts after `after`, which is empty for all but the range the boundary of
// the already-emitted first page falls inside.
type keyRange struct {
	prefix string
	after  string
}

// pageFunc turns a listing page into the nodes the walk deals in. It runs on
// whichever goroutine fetched the page, so a divided listing builds its nodes
// in parallel as well — and what waits in a channel is then a node apiece
// rather than the response the SDK parsed it out of, which is several times
// the size.
type pageFunc func([]*container.BlobItem) []*store.Node

// listUnder hands every blob under prefix to emit, a page at a time, dividing
// what is left once the listing has gone on long enough to be worth it.
func (s *Store) listUnder(ctx context.Context, cc *container.Client, prefix string,
	nodes pageFunc, emit func([]*store.Node) error) error {

	pager := s.flatPager(cc, prefix)
	started := time.Now()
	var last string
	for page := 1; ; page++ {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		items := pageItems(resp)
		if err := emit(nodes(items)); err != nil {
			return err
		}
		if !pager.More() {
			return nil
		}
		if name := lastName(items); name != "" {
			last = name
		}
		if page >= splitAfterPages || time.Since(started) > splitAfterTime {
			break
		}
	}

	// Everything up to here has been emitted, so a range lying wholly below it
	// has nothing left to say and is never listed.
	ranges := s.divide(ctx, cc, prefix, last)
	if len(ranges) < 2 {
		return drainPages(ctx, pager, nodes, emit)
	}
	s.log.Debug("dividing a listing that is more than one page",
		"prefix", prefix, "ranges", len(ranges))
	return s.listRanges(ctx, cc, ranges, nodes, emit)
}

func (s *Store) flatPager(cc *container.Client, prefix string) *blobPager {
	return cc.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{
		Prefix:  &prefix,
		Include: container.ListBlobsInclude{Metadata: s.cfg.IncludeMetadata},
	})
}

func pageItems(page container.ListBlobsFlatResponse) []*container.BlobItem {
	if page.Segment == nil {
		return nil
	}
	return page.Segment.BlobItems
}

// lastName is the highest-sorting name a page carried, which is how much of the
// container has already been dealt with.
func lastName(items []*container.BlobItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Name != nil {
			return *items[i].Name
		}
	}
	return ""
}

// drainPages finishes a listing the ordinary way, one page after another.
func drainPages(ctx context.Context, pager *blobPager, nodes pageFunc,
	emit func([]*store.Node) error) error {

	for pager.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		if err := emit(nodes(pageItems(page))); err != nil {
			return err
		}
	}
	return nil
}

// listRanges lists the ranges side by side, handing on each page as it lands.
//
// They are not handed on in range order, and that is the point. Ordering them
// would mean the range being consumed is the only one allowed to get ahead —
// every other listing fills the page it is allowed to hold and then stops
// fetching, which is the serial listing again with more goroutines. Buying the
// order back with buffers is not on offer either: a page is five thousand
// parsed blobs, and holding a whole container's worth of them is a
// several-hundred-megabyte answer to a problem about round trips.
//
// Nothing downstream asks for more than it gets. `Store.WalkAll` promises no
// order, and what the walk really has to hold to — ancestors before their
// contents, which the empty-directory markers and the destination directories
// both depend on — is a property of each blob's own emission, not of the
// sequence. Containers still arrive one after another, in the order they were
// listed.
func (s *Store) listRanges(ctx context.Context, cc *container.Client, ranges []keyRange,
	nodes pageFunc, emit func([]*store.Node) error) error {

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Slots are taken without waiting, so a walk that is already using its
	// whole allowance somewhere else simply carries on undivided rather than
	// waiting on itself.
	sem := s.splitSlots()
	workers := 0
	for workers < len(ranges) {
		select {
		case sem <- struct{}{}:
			workers++
			continue
		default:
		}
		break
	}
	defer func() {
		for range workers {
			<-sem
		}
	}()
	if workers == 0 {
		for _, r := range ranges {
			if err := s.listPages(ctx, cc, r, nodes, emit); err != nil {
				return err
			}
		}
		return nil
	}

	queue := make(chan keyRange)
	// One page in hand and one being fetched per worker is all the slack the
	// consumer needs: reading a page takes milliseconds and fetching one takes
	// seconds, so a worker is never waiting here for long.
	pages := make(chan []*store.Node, 1)

	var failed sync.Once
	var listErr error
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for r := range queue {
				err := s.listPages(ctx, cc, r, nodes, func(page []*store.Node) error {
					select {
					case pages <- page:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				if err != nil {
					failed.Do(func() { listErr = err })
					cancel()
					return
				}
			}
		})
	}
	go func() {
		defer close(queue)
		for _, r := range ranges {
			select {
			case queue <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(pages)
	}()

	var emitErr error
	for page := range pages {
		if emitErr != nil {
			// Drained rather than abandoned, so no worker is left blocked on a
			// channel nobody is reading.
			continue
		}
		if err := emit(page); err != nil {
			emitErr = err
			cancel()
		}
	}
	if emitErr != nil {
		return emitErr
	}
	return listErr
}

// listPages reads one range to the end.
func (s *Store) listPages(ctx context.Context, cc *container.Client, r keyRange,
	nodes pageFunc, emit func([]*store.Node) error) error {

	pager := s.flatPager(cc, r.prefix)
	after := r.after
	for pager.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := pager.NextPage(ctx)
		if err != nil {
			return err
		}
		items := pageItems(page)
		if after != "" {
			items = dropThrough(items, after)
			if len(items) > 0 {
				// Names only ever climb, so once the boundary is behind us it
				// stays behind us.
				after = ""
			}
		}
		if len(items) == 0 {
			continue
		}
		if err := emit(nodes(items)); err != nil {
			return err
		}
	}
	return nil
}

// dropThrough discards the leading items that were already emitted.
func dropThrough(items []*container.BlobItem, after string) []*container.BlobItem {
	i, _ := slices.BinarySearchFunc(items, after, func(b *container.BlobItem, name string) int {
		if b.Name == nil {
			return -1
		}
		return strings.Compare(*b.Name, name)
	})
	// BinarySearchFunc lands on the first item not less than after; the one
	// equal to it has been emitted too.
	for i < len(items) && items[i].Name != nil && *items[i].Name == after {
		i++
	}
	return items[i:]
}

// divide works out how to cut everything under prefix into ranges that can be
// listed side by side. Fewer than two means the shape gave it nothing to work
// with, which is the caller's cue to carry on sequentially.
func (s *Store) divide(ctx context.Context, cc *container.Client,
	prefix, after string) []keyRange {

	ways := min(s.listAhead(), maxSplitWays)
	if ways < 2 {
		return nil
	}
	units := s.children(ctx, cc, prefix, prefix)
	for depth := 1; depth < maxSplitDepth && len(units) < ways; depth++ {
		grown := s.expand(ctx, cc, units, ways)
		if len(grown) <= len(units) {
			break
		}
		units = grown
	}
	if len(units) < 2 {
		return nil
	}
	return afterOnly(splitUnits(prefix, units, ways), after)
}

// children asks what lies immediately under prefix. Only the names matter, so
// this is a hierarchical listing without metadata — the cheapest question the
// API answers — and its failure is not the caller's problem: dividing is an
// optimisation, and the sequential listing it falls back to will report
// anything genuinely wrong.
//
// skip names the one entry that is not work to be done: the zero-byte marker
// standing for the prefix itself, which sorts before everything beneath it and
// so was emitted by the page that prompted the division.
func (s *Store) children(ctx context.Context, cc *container.Client, prefix, skip string) []string {
	pager := cc.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix: &prefix,
	})
	var out []string
	for pager.More() && len(out) <= maxSplitUnits {
		page, err := pager.NextPage(ctx)
		if err != nil {
			s.log.Debug("could not look under a prefix to divide it",
				"prefix", prefix, "error", err)
			return nil
		}
		if page.Segment == nil {
			continue
		}
		for _, p := range page.Segment.BlobPrefixes {
			if p.Name != nil {
				out = append(out, *p.Name)
			}
		}
		for _, b := range page.Segment.BlobItems {
			if b.Name != nil && *b.Name != skip {
				out = append(out, *b.Name)
			}
		}
	}
	// A hierarchical listing returns prefixes and blobs in separate groups;
	// the division below reads them as one sorted sequence.
	slices.Sort(out)
	return out
}

// expand replaces each unit with what lies under it, stopping as soon as there
// are enough to divide by. Leaving the rest closed costs nothing: opened up or
// not, the units still cover every key.
func (s *Store) expand(ctx context.Context, cc *container.Client, units []string, ways int) []string {
	out := make([]string, 0, len(units))
	for i, u := range units {
		// A blob has nothing under it, and once the remaining units would make
		// up the numbers there is nothing to gain by opening any more.
		if !strings.HasSuffix(u, "/") || len(out)+len(units)-i >= ways {
			out = append(out, u)
			continue
		}
		below := s.children(ctx, cc, u, "")
		if len(below) == 0 {
			out = append(out, u)
			continue
		}
		out = append(out, below...)
		if len(out) > maxSplitUnits {
			return append(out, units[i+1:]...)
		}
	}
	return out
}

// group is a set of units that one listing would cover, and the prefix that
// covers them.
type group struct {
	prefix string
	units  []string
	// whole records that the members share no byte to divide them by, so
	// nothing is gained by asking again.
	whole bool
}

// splitUnits cuts the units into at most ways prefixes which between them cover
// every key the units cover, and nothing else.
//
// The units are disjoint and none is a prefix of another, being one level of a
// hierarchical listing, so a group can be divided at the byte where its members
// first differ — and the parts, sharing a length and differing in that byte,
// cannot overlap. Dividing the largest group each time is what keeps a long
// shared prefix (every name under `subscriptions/`) from being mistaken for a
// shape that will not divide: the first cut only lengthens the prefix, and the
// second is the one that fans out.
func splitUnits(base string, units []string, ways int) []keyRange {
	groups := []*group{{prefix: base, units: units}}
	for len(groups) < ways {
		g := largestDivisible(groups)
		if g == nil {
			break
		}
		parts := divideGroup(g.units)
		if len(parts) < 2 || len(groups)+len(parts)-1 > maxSplitRanges {
			g.whole = true
			continue
		}
		next := make([]*group, 0, len(groups)+len(parts)-1)
		for _, o := range groups {
			if o == g {
				next = append(next, parts...)
				continue
			}
			next = append(next, o)
		}
		groups = next
	}

	out := make([]keyRange, 0, len(groups))
	for _, g := range groups {
		prefix := g.prefix
		if len(g.units) == 1 {
			// Nothing else lives under the group, so the unit's own name is a
			// tighter prefix for exactly the same keys.
			prefix = g.units[0]
		}
		out = append(out, keyRange{prefix: prefix})
	}
	return out
}

// largestDivisible picks the group to divide next by how many units it holds,
// which is the only estimate of its size available without asking.
func largestDivisible(groups []*group) *group {
	var best *group
	for _, g := range groups {
		if g.whole || len(g.units) < 2 {
			continue
		}
		if best == nil || len(g.units) > len(best.units) {
			best = g
		}
	}
	return best
}

// divideGroup cuts sorted, disjoint units at the byte where they first differ.
func divideGroup(units []string) []*group {
	shared := len(commonPrefix(units))
	for _, u := range units {
		if len(u) == shared {
			// A unit that is exactly the shared prefix cannot be told from
			// what lies under it, so the group stays whole.
			return nil
		}
	}
	var out []*group
	for i := 0; i < len(units); {
		j := i
		for j < len(units) && units[j][shared] == units[i][shared] {
			j++
		}
		out = append(out, &group{prefix: units[i][:shared+1], units: units[i:j]})
		i = j
	}
	return out
}

// commonPrefix is the longest prefix every unit shares. The units are sorted,
// so the first and the last are the only two that can disagree soonest.
func commonPrefix(units []string) string {
	if len(units) == 0 {
		return ""
	}
	first, last := units[0], units[len(units)-1]
	n := min(len(first), len(last))
	for i := range n {
		if first[i] != last[i] {
			return first[:i]
		}
	}
	return first[:n]
}

// afterOnly drops the ranges whose keys have all been emitted already and marks
// the one the boundary falls inside.
//
// A range whose prefix is not a prefix of `after` and sorts below it holds
// nothing else: every key in it begins with that prefix, so every key differs
// from `after` at the same byte and in the same direction.
func afterOnly(ranges []keyRange, after string) []keyRange {
	if after == "" {
		return ranges
	}
	out := ranges[:0]
	for _, r := range ranges {
		switch {
		case strings.HasPrefix(after, r.prefix):
			r.after = after
			out = append(out, r)
		case r.prefix < after:
		default:
			out = append(out, r)
		}
	}
	return out
}

// splitSlots is the run's allowance of divided listings, shared by every
// container so that an account of large ones cannot multiply the look-ahead by
// the fan-out.
func (s *Store) splitSlots() chan struct{} {
	s.splitOnce.Do(func() {
		s.splitSem = make(chan struct{}, min(s.listAhead(), maxSplitWays))
	})
	return s.splitSem
}
