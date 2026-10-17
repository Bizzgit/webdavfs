package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// rangeServer answers every ranged GET with whatever respond writes.
func rangeServer(t *testing.T, respond http.HandlerFunc) (*DavClient, *int32) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return testClient(srv.URL, 3), &n
}

func TestGetRangeRejectsWrongContentRange(t *testing.T) {
	d, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100") // we asked for 50-59
		w.WriteHeader(206)
		w.Write([]byte("0123456789"))
	})
	if data, err := d.GetRange(context.Background(), "/f", 50, 10); err == nil {
		t.Fatalf("accepted the wrong range as data: %q", data)
	}
}

func TestGetRangeRejectsMissingContentRange(t *testing.T) {
	d, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(206)
		w.Write([]byte("0123456789"))
	})
	if _, err := d.GetRange(context.Background(), "/f", 50, 10); err == nil {
		t.Fatal("accepted a 206 without Content-Range")
	}
}

// A short answer that is not the end of the file is not the end of the file.
func TestGetRangeShortAnswerBeforeEOFIsAnError(t *testing.T) {
	d, n := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-4/100")
		w.WriteHeader(206)
		w.Write([]byte("01234"))
	})
	if data, err := d.GetRange(context.Background(), "/f", 0, 10); err == nil {
		t.Fatalf("accepted 5 of 10 bytes in the middle of the file: %q", data)
	}
	if got := atomic.LoadInt32(n); got != 4 { // 1 try + 3 retries
		t.Fatalf("%d requests, want 4", got)
	}
}

func TestGetRangeShortAnswerAtEOFIsFine(t *testing.T) {
	d, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 95-99/100")
		w.WriteHeader(206)
		w.Write([]byte("56789"))
	})
	data, err := d.GetRange(context.Background(), "/f", 95, 10)
	if err != nil || string(data) != "56789" {
		t.Fatalf("got %q, %v", data, err)
	}
}

// A server that sends far more than the range: refused, never buffered.
func TestGetRangeRefusesMoreThanAsked(t *testing.T) {
	d, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-8388607/9999999999")
		w.WriteHeader(206)
		io.Copy(w, io.LimitReader(zeros{}, 8<<20))
	})
	if _, err := d.GetRange(context.Background(), "/f", 0, 4096); err == nil {
		t.Fatal("accepted 8 MB for a 4 KB range")
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) { return len(b), nil }

// An answer to a request without a range is capped as well.
func TestGetRefusesHugeAnswer(t *testing.T) {
	d, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, io.LimitReader(zeros{}, maxGetBody+1024))
	})
	if _, err := d.Get(context.Background(), "/f"); err == nil {
		t.Fatal("accepted an answer over the limit")
	}
}

// An error answer with a body must not hold on to the connection: with one
// connection allowed, a handful of 404s in a row still go through.
func TestErrorBodiesDoNotPinConnections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write(bytes.Repeat([]byte("x"), 200000)) // more than the client will drain
	}))
	t.Cleanup(srv.Close)
	d := &DavClient{Url: srv.URL, MaxConns: 1, MaxIdleConns: 1, Retries: 0, Timeout: 5 * time.Second, IdleTimeout: time.Minute}
	d.davSem = make(davSem, d.MaxConns)
	d.cc = &http.Client{Transport: d.newTransport()}
	// One context for all of them: cancelling the context of a request
	// would tear its connection down and hide the problem.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for i := 0; i < 6; i++ {
		_, err := d.GetRange(ctx, "/f", 0, 10)
		if !isErrno(err, syscall.ENOENT) {
			t.Fatalf("request %d: %v, want ENOENT (a hang means a pinned connection)", i, err)
		}
	}
}

func TestDrainBodyOnTruncatedBodyReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("abc"))
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { drainBody(resp); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainBody never returned")
	}
}

// do() and the body retry in GetRange share one budget: a server that drops
// connections and then breaks bodies costs Retries+1 requests, not the
// square of that.
func TestRetryBudgetIsSharedBetweenLayers(t *testing.T) {
	d, n := rangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(206)
		w.Write([]byte("012"))
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	})
	d.Retries = 3
	if _, err := d.GetRange(context.Background(), "/f", 0, 10); err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(n); got != 4 {
		t.Fatalf("%d requests, want 4 (1 + 3 retries)", got)
	}
}

func TestRetryWaitIsBounded(t *testing.T) {
	d := &DavClient{RetryDelay: 100 * time.Millisecond}
	for _, attempt := range []int{1, 5, 20, 38, 64, 200, 1 << 30} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		start := time.Now()
		d.retryWait(ctx, attempt)
		cancel()
		if time.Since(start) > time.Second {
			t.Fatalf("attempt %d: waited %v", attempt, time.Since(start))
		}
	}
	// the delay itself never exceeds the cap and never goes negative
	d.RetryDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if d.retryWait(ctx, 3) {
		t.Fatal("waited out an hour")
	}
}

func TestOptionLimits(t *testing.T) {
	for _, kv := range [][2]string{
		{"retries", "11"}, {"retries", "4000000000"}, {"retrydelay", "1h"}, {"timeout", "0"}, {"timeout", "500ms"},
	} {
		var m MountOptions
		if err := applyOption(&m, kv[0], kv[1]); err == nil {
			t.Fatalf("%s=%s should be rejected", kv[0], kv[1])
		}
	}
	var m MountOptions
	for _, kv := range [][2]string{{"retries", "10"}, {"retrydelay", "0"}, {"timeout", "1s"}, {"http2", ""}} {
		if err := applyOption(&m, kv[0], kv[1]); err != nil {
			t.Fatalf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	if !m.HTTP2 {
		t.Fatal("http2 option not set")
	}
}

func TestStrongETag(t *testing.T) {
	for in, want := range map[string]string{
		`"abc"`: `"abc"`, ` "abc" `: `"abc"`, `W/"abc"`: "", `abc`: "", "": "", `"`: "",
	} {
		if got := strongETag(in); got != want {
			t.Errorf("strongETag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPreconditionFailedIsESTALE(t *testing.T) {
	d, _ := rangeServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(412) })
	_, err := d.GetRangeIf(context.Background(), "/f", 0, 10, `"x"`)
	if !isErrno(err, syscall.ESTALE) {
		t.Fatalf("412 gave %v, want ESTALE", err)
	}
}

func TestHTTP2OnlyWhenAsked(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.Header().Set("X-Proto", fmt.Sprint(r.ProtoMajor))
		w.WriteHeader(206)
		w.Write([]byte("01234"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	for _, want := range []bool{false, true} {
		d := &DavClient{Url: srv.URL, HTTP2: want, TLSSkipVerify: true, MaxConns: 4, MaxIdleConns: 4,
			Retries: 1, Timeout: 5 * time.Second, IdleTimeout: time.Minute}
		d.davSem = make(davSem, d.MaxConns)
		d.cc = &http.Client{Transport: d.newTransport()}
		req, _ := d.buildRequest(context.Background(), "GET", "/f")
		req.Header.Set("Range", "bytes=0-4")
		resp, err := d.do(req)
		if err != nil {
			t.Fatalf("http2=%v: %v", want, err)
		}
		resp.Body.Close()
		if got := resp.ProtoMajor == 2; got != want {
			t.Fatalf("http2=%v but the connection used %s", want, resp.Proto)
		}
		data, err := d.GetRange(context.Background(), "/f", 0, 5)
		if err != nil || string(data) != "01234" {
			t.Fatalf("http2=%v: %q, %v", want, data, err)
		}
	}
	if !connectionDropped(fmt.Errorf("http2: server sent GOAWAY and closed the connection")) {
		t.Fatal("GOAWAY is not treated as a dropped connection")
	}
}

func TestTraceHeadersAreRedacted(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Basic c2VjcmV0")
	h.Set("Cookie", "session=secret")
	h.Set("Range", "bytes=0-9")
	out := tHeaders(h, " ")
	if strings.Contains(out, "c2VjcmV0") || strings.Contains(out, "session=secret") {
		t.Fatalf("credentials in trace output: %q", out)
	}
	if !strings.Contains(out, "bytes=0-9") {
		t.Fatalf("other headers lost: %q", out)
	}
}

func TestStatsLineAndLoop(t *testing.T) {
	statReads.Add(1)
	if l := statsLine(); !strings.HasPrefix(l, "stats: reads=") || !strings.Contains(l, "retries=") {
		t.Fatalf("stats line: %q", l)
	}
	stop := make(chan struct{})
	lines := make(chan string, 10)
	go statsLoop(20*time.Millisecond, stop, func(l string) { lines <- l })
	<-lines // first tick prints
	select {
	case l := <-lines:
		t.Fatalf("printed again with nothing changed: %q", l)
	case <-time.After(100 * time.Millisecond):
	}
	statReads.Add(1)
	select {
	case <-lines:
	case <-time.After(time.Second):
		t.Fatal("did not print after a change")
	}
	close(stop)
}
