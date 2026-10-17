package main

// Read buffer for sequential reads.
//
// The kernel never asks for more than 128 KB at a time, and every FUSE read
// used to become one HTTP GET of that size. A server with some latency per
// request (about 10 ms measured) then delivers little more than 13 MB/s on
// one connection, where 1 MB requests reach 32 MB/s. So when reads follow
// each other, a larger block is fetched in one GET and the next reads are
// answered from it.
//
// One raState belongs to one open file handle (fileHandle in fuse.go), so
// two readers of one file do not disturb each other.
//
// Rules, all deliberate:
//   - A read is only enlarged when it continues the previous one (starts at
//     most raSlack before or after where that ended). A seek, a strided or
//     backward access (ffprobe, mp4 atoms, mkv cues) is read as asked.
//   - Reads that arrive together (the kernel issues several with async_read)
//     share one fetch: the first creates the block, the rest wait for it.
//     The fetch runs on the handle's own context, so a reader that is
//     interrupted does not fail the fetch for the others.
//   - A block is only used for the version of the file it was read from
//     (size, modification time, ETag, and the generation that counts writes
//     and refused reads), and every fetch carries If-Match when there is an
//     ETag, so ranges of two versions of a file are never glued together.
//   - A request is always answered in full, so a read that crosses the end
//     of a block is put together from several pieces. A short answer would
//     be taken by the kernel as the end of the file.
//   - The next block is fetched in the background once the reader is past
//     the middle of a block, after two blocks in a row, and only while the
//     memory budget and the number of background fetches allow it.
//   - Memory is bounded per handle (readaheadblocks x readahead) and over
//     all handles (readaheadmax). Over budget, reads are simply not
//     buffered.
//   - If a block cannot be fetched, the reads that waited for it do their
//     own plain GET.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Set once at startup from the readahead* mount options.
var (
	readAheadSize   int64 = 1 << 20   // bytes per fetched block, 0 turns the buffer off
	readAheadBlocks       = 4         // blocks kept per open file
	readAheadMax    int64 = 128 << 20 // bytes of buffer over all open files
)

// How far past the end of the previous read a read may start and still
// count as "the next one". Reads that the kernel issues in parallel can
// arrive slightly out of order. A read that starts before the end of the
// previous one never counts: backward and overlapping access is read as
// asked.
const raSlack = 256 << 10

var (
	raBytes       atomic.Int64 // memory reserved by blocks, all handles
	raPrefetching atomic.Int64 // background fetches in flight
	// raBg counts background fetches; tests wait for it.
	raBg sync.WaitGroup
)

var errPathChanged = errors.New("file was renamed during the read")

// raVer identifies the version of a file a block was read from.
type raVer struct {
	size  uint64
	mtime time.Time
	etag  string
	gen   uint64
}

func (a raVer) same(b raVer) bool {
	return a.size == b.size && a.mtime.Equal(b.mtime) && a.etag == b.etag && a.gen == b.gen
}

type raBlock struct {
	start int64
	end   int64 // end of the requested range; data may be shorter if the file shrank
	data  []byte
	err   error
	ready chan struct{} // closed when data or err is set
	ver   raVer
	used  uint64
}

func (b *raBlock) done() bool {
	select {
	case <-b.ready:
		return true
	default:
		return false
	}
}

func (b *raBlock) bytes() int64 { return b.end - b.start }

type raState struct {
	mu      sync.Mutex
	blocks  []*raBlock
	lastEnd int64 // end of the last read, -1 if none
	created int   // blocks started in this run of sequential reads
	tick    uint64
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
}

func newRaState() *raState {
	ctx, cancel := context.WithCancel(context.Background())
	return &raState{lastEnd: -1, ctx: ctx, cancel: cancel}
}

// close drops every block and cancels the fetches still running. The handle
// is not used after this.
func (ra *raState) close() {
	ra.cancel()
	ra.mu.Lock()
	ra.closed = true
	ra.unlinkAll()
	ra.mu.Unlock()
}

// unlink removes blocks[i] and gives its memory back. ra.mu held.
func (ra *raState) unlink(i int) {
	raBytes.Add(-ra.blocks[i].bytes())
	ra.blocks = append(ra.blocks[:i], ra.blocks[i+1:]...)
}

func (ra *raState) unlinkAll() {
	for len(ra.blocks) > 0 {
		ra.unlink(len(ra.blocks) - 1)
	}
	ra.lastEnd, ra.created = -1, 0
}

// remove unlinks b if it is still in the list.
func (ra *raState) remove(b *raBlock) {
	ra.mu.Lock()
	for i, o := range ra.blocks {
		if o == b {
			ra.unlink(i)
			break
		}
	}
	ra.mu.Unlock()
}

// find returns the block that covers pos, dropping blocks of another
// version of the file on the way. ra.mu held.
func (ra *raState) find(pos int64, ver raVer) *raBlock {
	var hit *raBlock
	for i := 0; i < len(ra.blocks); {
		b := ra.blocks[i]
		if !b.ver.same(ver) {
			ra.unlink(i)
			continue
		}
		if hit == nil && b.start <= pos && pos < b.end {
			hit = b
		}
		i++
	}
	return hit
}

// evict removes the least recently used finished block, if any. ra.mu held.
func (ra *raState) evict() bool {
	victim := -1
	for i, o := range ra.blocks {
		if o.done() && (victim < 0 || o.used < ra.blocks[victim].used) {
			victim = i
		}
	}
	if victim < 0 {
		return false
	}
	ra.unlink(victim)
	return true
}

// add puts a new in-flight block for [start, start+readAheadSize) in the
// list, or returns nil if there is nothing to fetch there or the memory
// budget does not allow it. ra.mu held.
func (ra *raState) add(start int64, ver raVer) *raBlock {
	end := start + readAheadSize
	if end > int64(ver.size) {
		end = int64(ver.size)
	}
	if ra.closed || end <= start {
		return nil
	}
	for len(ra.blocks) >= readAheadBlocks && ra.evict() {
	}
	if len(ra.blocks) >= readAheadBlocks {
		return nil // everything in flight
	}
	for raBytes.Load()+(end-start) > readAheadMax && ra.evict() {
	}
	if raBytes.Load()+(end-start) > readAheadMax {
		return nil
	}
	b := &raBlock{start: start, end: end, ready: make(chan struct{}), ver: ver, used: ra.tick}
	raBytes.Add(b.bytes())
	ra.blocks = append(ra.blocks, b)
	return b
}

// fetch starts the GET for b in the background and wakes everyone waiting
// for it when it is done.
func (nf *Node) raFetch(ra *raState, b *raBlock, path string, prefetch bool) {
	raBg.Add(1)
	go func() {
		defer raBg.Done()
		if prefetch {
			defer raPrefetching.Add(-1)
			statPrefetch.Add(1)
		}
		if trace(T_FUSE) {
			tPrintf("readahead %s: fetch %d-%d", nf.Name, b.start, b.end)
		}
		data, err := nf.rangeIf(ra.ctx, path, b.start, int(b.bytes()), b.ver.etag)
		if err == nil {
			// The node may have been renamed or removed while we fetched.
			nf.Lock()
			if nf.Deleted || nf.getPath() != path {
				err = errPathChanged
			}
			nf.Unlock()
		}
		b.data, b.err = data, err
		close(b.ready)
		if err != nil {
			ra.remove(b)
		}
	}()
}

// rangeIf reads a range guarded by the ETag the node had when the read
// started, and takes care of the bookkeeping if the server says the file is
// not that version any more.
func (nf *Node) rangeIf(ctx context.Context, path string, off int64, n int, etag string) ([]byte, error) {
	data, err := dav.GetRangeIf(ctx, path, off, n, etag)
	if err != nil && isStale(err) {
		nf.raStale()
	}
	return data, err
}

func isStale(err error) bool {
	var de *DavError
	return errors.As(err, &de) && de.Code == 412
}

// raStale is called when the server says the file is not what we think it
// is: every buffered block of every handle becomes invalid and the next
// look at the file asks the server again.
func (nf *Node) raStale() {
	statStale.Add(1)
	atomic.AddUint64(&nf.gen, 1)
	nf.Lock()
	nf.LastStat = time.Time{}
	nf.Unlock()
}

// raPrefetch starts the block after b in the background if nobody has yet.
func (nf *Node) raPrefetch(ra *raState, b *raBlock, path string, ver raVer) {
	if b.end >= int64(ver.size) {
		return
	}
	limit := int64(dav.MaxConns / 4)
	if limit < 1 {
		limit = 1
	}
	ra.mu.Lock()
	if ra.created < 2 || ra.find(b.end, ver) != nil {
		ra.mu.Unlock()
		return
	}
	if raPrefetching.Add(1) > limit {
		raPrefetching.Add(-1)
		ra.mu.Unlock()
		return
	}
	ra.tick++
	nb := ra.add(b.end, ver)
	if nb == nil {
		raPrefetching.Add(-1)
	}
	ra.mu.Unlock()
	if nb != nil {
		nf.raFetch(ra, nb, path, true)
	}
}

// raPiece returns the data at pos from a buffered or newly fetched block,
// at most up to end. Fewer bytes than asked is normal (the block ended);
// none at all means the end of the file. grow says whether a block may be
// fetched here, which is only so for sequential reads. A new block starts at
// from rather than pos: reads that arrive together can come in any order,
// and starting at the end of the previous read lets all of them find it.
func (nf *Node) raPiece(ctx context.Context, ra *raState, path string, pos, end, from int64, ver raVer, grow bool) ([]byte, error) {
	if pos >= int64(ver.size) {
		return nil, nil
	}
	plain := func() ([]byte, error) {
		statPlain.Add(1)
		return nf.rangeIf(ctx, path, pos, int(end-pos), ver.etag)
	}
	ra.mu.Lock()
	ra.tick++
	b := ra.find(pos, ver)
	if b == nil {
		if grow {
			if b = ra.add(from, ver); b != nil {
				ra.created++
				nf.raFetch(ra, b, path, false)
			}
		}
		if b == nil {
			ra.mu.Unlock()
			return plain()
		}
	}
	b.used = ra.tick
	ra.mu.Unlock()

	select {
	case <-b.ready:
	case <-ctx.Done():
		return nil, EINTR
	}
	if b.err != nil {
		if isStale(b.err) {
			return nil, b.err
		}
		// The block we waited for failed; do our own plain read.
		return plain()
	}

	rel := pos - b.start
	if rel < 0 || rel >= int64(len(b.data)) {
		return nil, nil // the file ends here
	}
	hi := b.bytes()
	if hi > int64(len(b.data)) {
		hi = int64(len(b.data))
	}
	if end-b.start < hi {
		hi = end - b.start
	}
	if hi <= rel {
		return nil, nil
	}
	statBufHits.Add(1)
	if grow && int64(len(b.data)) == b.bytes() && hi > b.bytes()/2 {
		nf.raPrefetch(ra, b, path, ver)
	}
	return b.data[rel:hi], nil
}

// readAhead answers a read of n bytes at off in a file of version ver.
func (nf *Node) readAhead(ctx context.Context, ra *raState, path string, off int64, n int, ver raVer) ([]byte, error) {
	end := off + int64(n)
	if end > int64(ver.size) {
		end = int64(ver.size)
	}
	if off >= end {
		return []byte{}, nil
	}
	n = int(end - off)
	if readAheadSize <= 0 || readAheadBlocks <= 0 || int64(n) >= readAheadSize {
		statPlain.Add(1)
		return nf.rangeIf(ctx, path, off, n, ver.etag)
	}

	// Decide, and claim the stretch at once: reads that arrive a moment
	// later (the kernel sends several together) then see where this one
	// ends.
	ra.mu.Lock()
	lastEnd := ra.lastEnd
	seq := lastEnd >= 0 && off >= lastEnd && off <= lastEnd+raSlack
	from := off
	if seq {
		from = lastEnd
		ra.lastEnd = end
	} else {
		ra.lastEnd, ra.created = end, 0
	}
	ra.mu.Unlock()

	var pieces [][]byte
	pos := off
	for pos < end {
		if pos != off {
			from = pos
		}
		p, err := nf.raPiece(ctx, ra, path, pos, end, from, ver, seq)
		if err != nil {
			return nil, err
		}
		if len(p) == 0 {
			break
		}
		pieces = append(pieces, p)
		pos += int64(len(p))
	}

	switch len(pieces) {
	case 0:
		return []byte{}, nil
	case 1:
		return pieces[0], nil
	}
	out := make([]byte, 0, pos-off)
	for _, p := range pieces {
		out = append(out, p...)
	}
	return out, nil
}
