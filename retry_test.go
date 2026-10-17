package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyServer drops the connection without an answer for the first "drops"
// requests, then answers 206 with "0123456789". It counts every request and
// remembers the body of the last one.
type flakyServer struct {
	*httptest.Server
	hits     int32
	lastBody atomic.Value
}

func newFlakyServer(t *testing.T, drops int32, handler http.HandlerFunc) *flakyServer {
	fs := &flakyServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&fs.hits, 1)
		b, _ := io.ReadAll(r.Body)
		fs.lastBody.Store(string(b))
		if n <= drops {
			c, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				c.Close()
			}
			return
		}
		handler(w, r)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func okHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Range", "bytes 0-9/10")
	w.Header().Set("Content-Length", "10")
	w.WriteHeader(206)
	io.WriteString(w, "0123456789")
}

func testClient(url string, retries int) *DavClient {
	return &DavClient{
		Url:        url,
		Retries:    retries,
		RetryDelay: time.Millisecond,
		cc: &http.Client{
			// A fresh connection per request, so every attempt
			// is a real new connection and the transport's own
			// retry of reused connections does not hide ours.
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

func TestGetRangeRetriesDroppedConnection(t *testing.T) {
	srv := newFlakyServer(t, 2, okHandler)
	d := testClient(srv.URL, 3)
	data, err := d.GetRange(context.Background(), "/f", 0, 10)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	if string(data) != "0123456789" {
		t.Fatalf("data = %q", data)
	}
	if h := atomic.LoadInt32(&srv.hits); h != 3 {
		t.Fatalf("server saw %d requests, want 3", h)
	}
}

func TestGetRangeGivesUpAfterRetries(t *testing.T) {
	srv := newFlakyServer(t, 100, okHandler)
	d := testClient(srv.URL, 2)
	_, err := d.GetRange(context.Background(), "/f", 0, 10)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !connectionDropped(err) {
		t.Fatalf("error %v is not a dropped connection", err)
	}
	if h := atomic.LoadInt32(&srv.hits); h != 3 {
		t.Fatalf("server saw %d requests, want 3 (1 try + 2 retries)", h)
	}
}

func TestRetriesZeroMeansNoRetry(t *testing.T) {
	srv := newFlakyServer(t, 1, okHandler)
	d := testClient(srv.URL, 0)
	if _, err := d.GetRange(context.Background(), "/f", 0, 10); err == nil {
		t.Fatal("expected an error with retries=0")
	}
	if h := atomic.LoadInt32(&srv.hits); h != 1 {
		t.Fatalf("server saw %d requests, want 1", h)
	}
}

// A connection that dies halfway through the body is retried as well.
func TestGetRangeRetriesTruncatedBody(t *testing.T) {
	var calls int32
	srv := newFlakyServer(t, 0, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Range", "bytes 0-9/10")
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(206)
			io.WriteString(w, "012")
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		}
		okHandler(w, r)
	})
	d := testClient(srv.URL, 3)
	data, err := d.GetRange(context.Background(), "/f", 0, 10)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	if string(data) != "0123456789" {
		t.Fatalf("data = %q", data)
	}
}

// PROPFIND carries a body; the retry must send it again.
func TestPropFindRetryResendsBody(t *testing.T) {
	const reply = `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href>` +
		`<D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>` +
		`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	srv := newFlakyServer(t, 1, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(207)
		io.WriteString(w, reply)
	})
	d := testClient(srv.URL, 3)
	if _, err := d.PropFind(context.Background(), "/", 0, nil); err != nil {
		t.Fatalf("PropFind: %v", err)
	}
	if h := atomic.LoadInt32(&srv.hits); h != 2 {
		t.Fatalf("server saw %d requests, want 2", h)
	}
	if body, _ := srv.lastBody.Load().(string); !strings.Contains(body, "propfind") {
		t.Fatalf("retry sent body %q, want the PROPFIND XML", body)
	}
}

// A write that failed may have been applied, so it must never be repeated.
func TestWritesAreNotRetried(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "MOVE"} {
		srv := newFlakyServer(t, 1, okHandler)
		d := testClient(srv.URL, 3)
		req, err := d.buildRequest(context.Background(), method, "/f", "data")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.do(req); err == nil {
			t.Fatalf("%s: expected an error", method)
		}
		if h := atomic.LoadInt32(&srv.hits); h != 1 {
			t.Fatalf("%s: server saw %d requests, want 1", method, h)
		}
	}
}

func TestHTTPErrorStatusIsNotRetried(t *testing.T) {
	srv := newFlakyServer(t, 0, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	d := testClient(srv.URL, 3)
	if _, err := d.GetRange(context.Background(), "/f", 0, 10); err == nil {
		t.Fatal("expected an error for 404")
	}
	if h := atomic.LoadInt32(&srv.hits); h != 1 {
		t.Fatalf("server saw %d requests, want 1", h)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	srv := newFlakyServer(t, 100, okHandler)
	d := testClient(srv.URL, 50)
	d.RetryDelay = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.GetRange(ctx, "/f", 0, 10); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v, retrying did not stop with the context", time.Since(start))
	}
	if h := atomic.LoadInt32(&srv.hits); h >= 50 {
		t.Fatalf("server saw %d requests, the context did not stop the retries", h)
	}
}

func TestConnectionDropped(t *testing.T) {
	if connectionDropped(nil) {
		t.Fatal("nil is not a dropped connection")
	}
	if !connectionDropped(io.EOF) || !connectionDropped(io.ErrUnexpectedEOF) {
		t.Fatal("EOF must count as dropped")
	}
	if connectionDropped(errors.New("400 Will not follow redirect")) {
		t.Fatal("an ordinary error is not a dropped connection")
	}
	if connectionDropped(context.DeadlineExceeded) {
		t.Fatal("a timeout is not retried")
	}
}

func TestRetryOptions(t *testing.T) {
	var mo MountOptions
	for _, kv := range [][2]string{
		{"retries", "0"}, {"retrydelay", "250ms"}, {"idletimeout", "2m"},
		{"timeout", "30"}, {"statcache", "0"}, {"attrtimeout", "5m"},
	} {
		if err := applyOption(&mo, kv[0], kv[1]); err != nil {
			t.Fatalf("%s=%s: %v", kv[0], kv[1], err)
		}
	}
	if !mo.Retries.Set || mo.Retries.N != 0 {
		t.Fatalf("retries=0 must be set and zero: %+v", mo.Retries)
	}
	if mo.RetryDelay.D != 250*time.Millisecond || mo.IdleTimeout.D != 2*time.Minute {
		t.Fatalf("durations wrong: %+v %+v", mo.RetryDelay, mo.IdleTimeout)
	}
	if mo.Timeout.D != 30*time.Second {
		t.Fatalf("a bare number is seconds, got %v", mo.Timeout.D)
	}
	if !mo.StatCache.Set || mo.StatCache.D != 0 {
		t.Fatalf("statcache=0 must be set and zero: %+v", mo.StatCache)
	}
	if mo.AttrTimeout.D != 5*time.Minute {
		t.Fatalf("attrtimeout = %v", mo.AttrTimeout.D)
	}
	for _, kv := range [][2]string{
		{"retries", "x"}, {"retries", "-1"}, {"retrydelay", "soon"}, {"timeout", "-5s"}, {"statcache", ""},
	} {
		var bad MountOptions
		if err := applyOption(&bad, kv[0], kv[1]); err == nil {
			t.Fatalf("%s=%q should be rejected", kv[0], kv[1])
		}
	}
	var unset MountOptions
	if unset.Retries.Set || unset.StatCache.Set {
		t.Fatal("options must start out unset")
	}
}
