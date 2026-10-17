package main

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Counters for what the client does, printed once a minute but only when
// something changed, so a quiet mount adds nothing to the trace file and a
// busy one adds one short line. They are the way to see in production that
// retries and the read buffer work, without -T output.
var (
	statReads       atomic.Int64 // FUSE reads on a file handle
	statBufHits     atomic.Int64 // pieces answered from the read buffer
	statPlain       atomic.Int64 // pieces fetched without the buffer
	statGets        atomic.Int64 // GET requests that got an answer
	statBytes       atomic.Int64 // bytes received for those
	statPrefetch    atomic.Int64 // blocks fetched in the background
	statRetries     atomic.Int64 // retries after a dropped connection
	statRetryGaveUp atomic.Int64 // reads that ran out of retries
	statStale       atomic.Int64 // 412: file changed on the server
	statReadErrors  atomic.Int64 // reads that returned an error
)

func statsLine() string {
	return fmt.Sprintf("stats: reads=%d buffer_hits=%d plain=%d gets=%d recv_mb=%d prefetch=%d retries=%d gave_up=%d stale=%d errors=%d buffered_mb=%d",
		statReads.Load(), statBufHits.Load(), statPlain.Load(), statGets.Load(), statBytes.Load()>>20,
		statPrefetch.Load(), statRetries.Load(), statRetryGaveUp.Load(), statStale.Load(),
		statReadErrors.Load(), raBytes.Load()>>20)
}

// statsLoop prints statsLine every interval if it differs from the last one
// printed. It returns when stop is closed.
func statsLoop(interval time.Duration, stop <-chan struct{}, print func(string)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if l := statsLine(); l != last {
				print(l)
				last = l
			}
		}
	}
}
