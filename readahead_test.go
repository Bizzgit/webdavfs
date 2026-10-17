package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"bazil.org/fuse"
)

const raFileSize = 8 << 20

// raEnv is a file of known content on a test server that counts what is
// asked of it and can be told to misbehave.
type raEnv struct {
	mu      sync.Mutex
	content []byte
	etag    string
	hook    func(w http.ResponseWriter, r *http.Request) bool // true: handled, do not serve
	reqs    int32
	served  int64
	node    *Node
	handles []*fileHandle
}

func newRaEnv(t *testing.T) *raEnv {
	e := &raEnv{content: make([]byte, raFileSize), etag: `"v1"`}
	for i := range e.content {
		e.content[i] = byte(i*7 + i>>8 + i>>17)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&e.reqs, 1)
		e.mu.Lock()
		content, etag, hook := e.content, e.etag, e.hook
		e.mu.Unlock()
		if hook != nil && hook(w, r) {
			return
		}
		w.Header().Set("ETag", etag)
		http.ServeContent(&countWriter{ResponseWriter: w, n: &e.served}, r, "f", time.Time{}, bytes.NewReader(content))
	}))

	oldDav, oldSize, oldBlocks, oldMax := dav, readAheadSize, readAheadBlocks, readAheadMax
	dav = testClient(srv.URL, 0)
	dav.MaxConns = 8
	dav.davSem = make(davSem, dav.MaxConns)
	readAheadSize, readAheadBlocks, readAheadMax = 1<<20, 4, 128<<20
	e.node = &Node{
		Dnode:  Dnode{Name: "f", Size: raFileSize, Mtime: time.Unix(1000, 0), ETag: `"v1"`},
		Parent: rootNode,
	}
	t.Cleanup(func() {
		for _, h := range e.handles {
			h.ra.close()
		}
		raBg.Wait()
		srv.Close()
		if b := raBytes.Load(); b != 0 {
			t.Errorf("%d bytes of read buffer leaked", b)
		}
		dav, readAheadSize, readAheadBlocks, readAheadMax = oldDav, oldSize, oldBlocks, oldMax
	})
	return e
}

type countWriter struct {
	http.ResponseWriter
	n *int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	atomic.AddInt64(c.n, int64(len(b)))
	return c.ResponseWriter.Write(b)
}

func (e *raEnv) open() *fileHandle {
	h := &fileHandle{Node: e.node, ra: newRaState()}
	e.handles = append(e.handles, h)
	return h
}

func (e *raEnv) requests() int32 { return atomic.LoadInt32(&e.reqs) }

// rdCtx does a read through the real Node.read path and returns the result.
func (e *raEnv) rdCtx(ctx context.Context, h *fileHandle, off int64, n int) ([]byte, error) {
	resp := &fuse.ReadResponse{}
	err := h.Read(ctx, &fuse.ReadRequest{Offset: off, Size: n}, resp)
	return resp.Data, err
}

// rd reads and checks the data against the file.
func (e *raEnv) rd(t *testing.T, h *fileHandle, off int64, n int) []byte {
	t.Helper()
	got, err := e.rdCtx(context.Background(), h, off, n)
	if err != nil {
		t.Fatalf("read(%d, %d): %v", off, n, err)
	}
	end := off + int64(n)
	if end > raFileSize {
		end = raFileSize
	}
	e.mu.Lock()
	want := e.content[off:end]
	e.mu.Unlock()
	if !bytes.Equal(got, want) {
		t.Fatalf("read(%d, %d): wrong data (%d bytes, want %d)", off, n, len(got), len(want))
	}
	return got
}

func isErrno(err error, no syscall.Errno) bool {
	var en fuse.ErrorNumber
	return errors.As(err, &en) && en.Errno() == fuse.Errno(no)
}

func TestReadAheadSequentialUsesFewRequests(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	for off := int64(0); off < raFileSize; off += 128 << 10 {
		e.rd(t, h, off, 128<<10)
	}
	raBg.Wait()
	// 64 reads of 128 KB. Without the buffer that is 64 requests.
	if r := e.requests(); r > 12 {
		t.Fatalf("%d requests for a sequential read of 8 MB, want about 8", r)
	}
	if s := atomic.LoadInt64(&e.served); s > raFileSize+2<<20 {
		t.Fatalf("server sent %d bytes for an 8 MB file", s)
	}
}

func TestReadAheadRandomStridedAndBackwardReadsAreNotEnlarged(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	var want int64
	rd := func(off int64) { e.rd(t, h, off, 64<<10); want += 64 << 10 }
	for _, off := range []int64{6 << 20, 1 << 20, 4 << 20, 100 << 10, 7 << 20} { // random
		rd(off)
	}
	for i := int64(0); i < 8; i++ { // strided: 64K every 512K
		rd(2<<20 + i*(512<<10) - 0)
	}
	for i := int64(0); i < 8; i++ { // backward: 64K every 128K back
		rd(7<<20 - i*(128<<10))
	}
	if s := atomic.LoadInt64(&e.served); s != want {
		t.Fatalf("server sent %d bytes for reads that total %d: reads were enlarged", s, want)
	}
}

func TestReadAheadParallelReadsInAnyOrderShareAFetch(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		e := newRaEnv(t)
		h := e.open()
		e.rd(t, h, 0, 128<<10) // the first read of a sequence is plain
		// the kernel sends reads in order; scheduling can swap neighbours
		order := []int{0, 1, 2, 3, 4, 5, 6}
		rnd := rand.New(rand.NewSource(int64(trial)))
		for k := 0; k+1 < len(order); k += 2 {
			if rnd.Intn(2) == 0 {
				order[k], order[k+1] = order[k+1], order[k]
			}
		}
		var wg sync.WaitGroup
		for _, i := range order {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e.rd(t, h, int64(i+1)*(128<<10), 128<<10)
			}(i)
			time.Sleep(300 * time.Microsecond) // they arrive one after the other
		}
		wg.Wait()
		raBg.Wait()
		// plain first read, then at most a few fetches for the rest
		if r := e.requests(); r > 4 {
			t.Fatalf("trial %d: %d requests, parallel reads did not share fetches", trial, r)
		}
		for _, hh := range e.handles {
			hh.ra.close()
		}
		raBg.Wait()
	}
}

func TestReadAheadReadAcrossBlockEnd(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	e.rd(t, h, 4096, 4096) // starts a block at 4096, ends at 4096+1M
	e.rd(t, h, 4096+(1<<20)-1000, 8192)
	e.rd(t, h, 4096+(1<<20)+7192, 300<<10)
}

func TestReadAheadEndOfFile(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, raFileSize-300<<10, 128<<10)
	e.rd(t, h, raFileSize-172<<10, 128<<10)
	got := e.rd(t, h, raFileSize-44<<10, 128<<10) // asks past the end, gets the rest
	if len(got) != 44<<10 {
		t.Fatalf("got %d bytes at the end, want %d", len(got), 44<<10)
	}
	if got, err := e.rdCtx(context.Background(), h, raFileSize, 4096); err != nil || len(got) != 0 {
		t.Fatalf("read at EOF: %d bytes, %v", len(got), err)
	}
}

// The file shrinks between the moment Read looked at its size and the moment
// the buffer did: this used to panic and take the mount down.
func TestReadAheadFileShrinksDuringRead(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 5<<20, 4096)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e.node.Lock()
			if i%2 == 0 {
				e.node.Size = 100 << 10
			} else {
				e.node.Size = raFileSize
			}
			e.node.Unlock()
		}
	}()
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		off := int64(5<<20) + int64(i%64)*4096
		if i%7 == 0 {
			off = rnd.Int63n(raFileSize)
		}
		e.rdCtx(context.Background(), h, off, 4096) // errors are fine, a panic is not
	}
	close(stop)
	// and directly, with a version older than the read
	ver := raVer{size: 100 << 10, mtime: e.node.Mtime, etag: `"v1"`}
	if got, err := e.node.readAhead(context.Background(), h.ra, "/f", 5<<20, 4096, ver); err != nil || len(got) != 0 {
		t.Fatalf("read past the end of the known size: %d bytes, %v", len(got), err)
	}
}

func TestReadAheadDroppedWhenFileChanges(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	e.rd(t, h, 4096, 4096)
	before := e.requests()
	e.rd(t, h, 8192, 4096) // from the buffer
	if e.requests() != before {
		t.Fatal("a read inside the block went to the server")
	}
	e.node.Lock()
	e.node.Mtime = time.Unix(2000, 0)
	e.node.Unlock()
	e.rd(t, h, 12288, 4096)
	if e.requests() == before {
		t.Fatal("a changed file was still read from the buffer")
	}
}

// A same-size, same-second replacement on the server: only the ETag shows
// it. The read must fail (ESTALE) and not return bytes of the new file next
// to bytes of the old one.
func TestReadAheadReplacedFileGivesESTALENotMixedData(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	e.rd(t, h, 4096, 4096) // block 4096..1M+4096 is buffered
	e.mu.Lock()
	old := e.content
	e.content = bytes.Repeat([]byte{0xAA}, raFileSize)
	e.etag = `"v2"`
	e.mu.Unlock()
	// still inside the buffer: old bytes, a consistent snapshot
	if got, err := e.rdCtx(context.Background(), h, 8192, 4096); err != nil || !bytes.Equal(got, old[8192:8192+4096]) {
		t.Fatalf("read inside the buffer after the replace: %v", err)
	}
	// beyond the buffer: the server must refuse, not answer with new bytes
	_, err := e.rdCtx(context.Background(), h, 3<<20, 4096)
	if err == nil || !isErrno(err, syscall.ESTALE) {
		t.Fatalf("read of a replaced file: %v, want ESTALE", err)
	}
	if statStale.Load() == 0 {
		t.Fatal("stale counter not incremented")
	}
	// the buffer is gone after that
	before := e.requests()
	e.rdCtx(context.Background(), h, 8192, 4096)
	if e.requests() == before {
		t.Fatal("buffer of a replaced file was kept")
	}
}

func TestReadAheadSendsIfMatch(t *testing.T) {
	e := newRaEnv(t)
	var got atomic.Value
	e.mu.Lock()
	e.hook = func(w http.ResponseWriter, r *http.Request) bool {
		got.Store(r.Header.Get("If-Match"))
		return false
	}
	e.mu.Unlock()
	e.rd(t, e.open(), 0, 4096)
	if v, _ := got.Load().(string); v != `"v1"` {
		t.Fatalf("If-Match = %q", v)
	}
	// no ETag known: no If-Match
	e.node.Lock()
	e.node.ETag = ""
	e.node.Unlock()
	e.rd(t, e.open(), 8192, 4096)
	if v, _ := got.Load().(string); v != "" {
		t.Fatalf("If-Match = %q without an ETag", v)
	}
}

func TestReadAheadGenerationDropsBuffers(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	e.rd(t, h, 4096, 4096)
	atomic.AddUint64(&e.node.gen, 1) // what Write and truncate do
	before := e.requests()
	e.rd(t, h, 8192, 4096)
	if e.requests() == before {
		t.Fatal("buffer survived a write")
	}
}

func TestReadAheadReleaseFreesMemoryAndStopsFetches(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	e.rd(t, h, 4096, 4096)
	if raBytes.Load() == 0 {
		t.Fatal("nothing buffered")
	}
	h.Release(context.Background(), nil)
	if b := raBytes.Load(); b != 0 {
		t.Fatalf("%d bytes still reserved after Release", b)
	}
	// a closed handle still reads, just without the buffer
	e.rd(t, h, 8192, 4096)
	if raBytes.Load() != 0 {
		t.Fatal("a closed handle buffered data")
	}
}

func TestReadAheadReleaseCancelsBackgroundFetch(t *testing.T) {
	e := newRaEnv(t)
	started := make(chan struct{}, 8)
	e.mu.Lock()
	e.hook = func(w http.ResponseWriter, r *http.Request) bool {
		var first, last int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last)
		if first >= 2<<20 { // only the prefetch of the third block
			started <- struct{}{}
			<-r.Context().Done() // hold it until the client gives up
			return true
		}
		return false
	}
	e.mu.Unlock()
	h := e.open()
	for off := int64(0); off < 2<<20+100<<10; off += 128 << 10 {
		e.rd(t, h, off, 128<<10)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no background fetch was started")
	}
	done := make(chan struct{})
	go func() { h.Release(context.Background(), nil); raBg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a background fetch survived Release")
	}
}

// Two readers of one file, 4 MB apart: each keeps its own position.
func TestReadAheadTwoHandlesDoNotDisturbEachOther(t *testing.T) {
	e := newRaEnv(t)
	a, b := e.open(), e.open()
	for i := int64(0); i < 16; i++ {
		e.rd(t, a, i*(128<<10), 128<<10)
		e.rd(t, b, 4<<20+i*(128<<10), 128<<10)
	}
	raBg.Wait()
	if r := e.requests(); r > 14 {
		t.Fatalf("%d requests for two sequential streams of 2 MB, want about 6", r)
	}
	// closing one handle must not hurt the other
	a.Release(context.Background(), nil)
	before := e.requests()
	e.rd(t, b, 4<<20+16*(128<<10), 128<<10)
	if e.requests() > before+1 {
		t.Fatal("closing one handle reset the other")
	}
}

func TestReadAheadStaysWithinMemoryBudget(t *testing.T) {
	e := newRaEnv(t)
	readAheadMax = 2<<20 + 1<<19 // room for two blocks over all handles
	hs := []*fileHandle{e.open(), e.open(), e.open()}
	for i := int64(0); i < 24; i++ {
		for k, h := range hs {
			e.rd(t, h, int64(k)*(2<<20)+i*(64<<10), 64<<10)
			if b := raBytes.Load(); b > readAheadMax {
				t.Fatalf("%d bytes reserved, budget is %d", b, readAheadMax)
			}
		}
	}
	raBg.Wait()
}

func TestReadAheadLimitsBlocksPerHandle(t *testing.T) {
	e := newRaEnv(t)
	readAheadBlocks = 2
	h := e.open()
	for off := int64(0); off < 6<<20; off += 256 << 10 {
		e.rd(t, h, off, 256<<10)
	}
	raBg.Wait()
	h.ra.mu.Lock()
	n := len(h.ra.blocks)
	h.ra.mu.Unlock()
	if n > 2 {
		t.Fatalf("%d blocks kept, want at most 2", n)
	}
}

func TestReadAheadPrefetchHappensAndIsCounted(t *testing.T) {
	e := newRaEnv(t)
	before := statPrefetch.Load()
	h := e.open()
	for off := int64(0); off < raFileSize; off += 128 << 10 {
		e.rd(t, h, off, 128<<10)
	}
	raBg.Wait()
	if statPrefetch.Load() == before {
		t.Fatal("no background fetch happened during a long sequential read")
	}
}

func TestReadAheadOffReadsPlain(t *testing.T) {
	e := newRaEnv(t)
	readAheadSize = 0
	h := e.open()
	for off := int64(0); off < 1<<20; off += 128 << 10 {
		e.rd(t, h, off, 128<<10)
	}
	if r := e.requests(); r != 8 {
		t.Fatalf("%d requests with readahead=0, want 8", r)
	}
}

// If the fetch of a block fails, the reads that waited for it do their own
// read and still succeed, and the failed block does not stay in the list.
func TestReadAheadFailedFetchWaitersFallBack(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	var once sync.Once
	e.mu.Lock()
	e.hook = func(w http.ResponseWriter, r *http.Request) bool {
		fail := false
		once.Do(func() { fail = true })
		if fail {
			time.Sleep(150 * time.Millisecond) // let the others queue up behind it
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
			return true
		}
		return false
	}
	e.mu.Unlock()
	var wg sync.WaitGroup
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.rd(t, h, int64(i)*4096, 4096) // every one of them must get its data
		}(i)
	}
	wg.Wait()
	raBg.Wait()
	h.ra.mu.Lock()
	for _, b := range h.ra.blocks {
		if b.done() && b.err != nil {
			t.Error("a failed block is still in the list")
		}
	}
	h.ra.mu.Unlock()
}

// The reader that started a fetch is interrupted: the others must still get
// the one shared fetch, not a plain read each.
func TestReadAheadInterruptedStarterDoesNotFailTheFetch(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	e.mu.Lock()
	e.hook = func(w http.ResponseWriter, r *http.Request) bool {
		time.Sleep(200 * time.Millisecond)
		return false
	}
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	starter := make(chan error, 1)
	go func() { _, err := e.rdCtx(ctx, h, 4096, 4096); starter <- err }()
	time.Sleep(50 * time.Millisecond)
	var wg sync.WaitGroup
	for i := 2; i <= 6; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); e.rd(t, h, int64(i)*4096, 4096) }(i)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-starter; err != EINTR {
		t.Fatalf("interrupted read: %v, want EINTR", err)
	}
	wg.Wait()
	raBg.Wait()
	if r := e.requests(); r > 3 {
		t.Fatalf("%d requests: the waiters did not share the fetch", r)
	}
}

func TestReadAheadRenameDuringReadStillWorks(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.rd(t, h, 0, 4096)
	var once sync.Once
	e.mu.Lock()
	e.hook = func(w http.ResponseWriter, r *http.Request) bool {
		once.Do(func() {
			e.node.Lock()
			e.node.Name = "g"
			e.node.Unlock()
		})
		return false
	}
	e.mu.Unlock()
	// the file was "f" when the read started and is "g" now; the server
	// serves the same content under any name in this test
	e.rd(t, h, 4096, 4096)
}

func TestReadAheadDeletedFileIsESTALE(t *testing.T) {
	e := newRaEnv(t)
	h := e.open()
	e.node.Lock()
	e.node.Deleted = true
	e.node.Unlock()
	_, err := e.rdCtx(context.Background(), h, 0, 4096)
	if !isErrno(err, syscall.ESTALE) {
		t.Fatalf("read of a deleted file: %v, want ESTALE", err)
	}
}

func TestReadAheadOptions(t *testing.T) {
	var mo MountOptions
	for _, kv := range [][2]string{{"readahead", "512K"}, {"readaheadblocks", "6"}, {"readaheadmax", "256M"}} {
		if err := applyOption(&mo, kv[0], kv[1]); err != nil {
			t.Fatalf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	if mo.ReadAhead.N != 512<<10 || mo.ReadAheadBlocks.N != 6 || mo.ReadAheadMax.N != 256<<20 {
		t.Fatalf("got %+v %+v %+v", mo.ReadAhead, mo.ReadAheadBlocks, mo.ReadAheadMax)
	}
	for v, want := range map[string]uint32{"2M": 2 << 20, "1048576": 1 << 20, "0": 0} {
		var m MountOptions
		if err := applyOption(&m, "readahead", v); err != nil || m.ReadAhead.N != want || !m.ReadAhead.Set {
			t.Fatalf("readahead=%s: %+v %v", v, m.ReadAhead, err)
		}
	}
	for _, kv := range [][2]string{
		{"readahead", ""}, {"readahead", "x"}, {"readahead", "-1"}, {"readahead", "100M"},
		{"readaheadblocks", "17"}, {"readaheadblocks", "-1"}, {"readaheadmax", "9G"},
	} {
		var m MountOptions
		if err := applyOption(&m, kv[0], kv[1]); err == nil {
			t.Fatalf("%s=%q should be rejected", kv[0], kv[1])
		}
	}
}

// Worst case: the reads of one batch arrive in any order at once. Nothing
// may break and it must never cost more requests than reading unbuffered.
func TestReadAheadParallelReadsInRandomOrderStayCorrect(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		e := newRaEnv(t)
		h := e.open()
		e.rd(t, h, 0, 128<<10)
		var wg sync.WaitGroup
		for _, i := range rand.New(rand.NewSource(int64(trial))).Perm(7) {
			wg.Add(1)
			go func(i int) { defer wg.Done(); e.rd(t, h, int64(i+1)*(128<<10), 128<<10) }(i)
		}
		wg.Wait()
		raBg.Wait()
		if r := e.requests(); r > 8 {
			t.Fatalf("trial %d: %d requests for 8 reads", trial, r)
		}
		h.ra.close()
		raBg.Wait()
	}
}
