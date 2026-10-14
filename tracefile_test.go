package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTraceFileMountOption(t *testing.T) {
	var mo MountOptions
	if err := applyOption(&mo, "tracefile", "/var/log/webdavfs.log"); err != nil {
		t.Fatal(err)
	}
	if mo.TraceFile != "/var/log/webdavfs.log" {
		t.Fatalf("TraceFile = %q", mo.TraceFile)
	}
	if err := applyOption(&mo, "tracefile", ""); err == nil {
		t.Fatal("tracefile without a value should be an error")
	}
}

// traceOpts with a file but without trace options must open the file (so the
// stdout/stderr redirect has a target) and trace nothing, and a file that
// cannot be opened must not be an error (the mount must still happen).
// One test function: the logger goroutines share the global trace channel.
func TestTraceOptsWithoutTraceOptions(t *testing.T) {
	fn := filepath.Join(t.TempDir(), "webdavfs.log")
	traceFile = nil
	if err := traceOpts("", fn); err != nil {
		t.Fatal(err)
	}
	if traceFile == nil {
		t.Fatal("trace file was not opened")
	}
	if traceOptions != 0 {
		t.Fatalf("traceOptions = %d, want 0", traceOptions)
	}
	tPrintf("hello from the test")
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fn)
		if strings.Contains(string(b), "hello from the test") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("line never reached the trace file, got %q", b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fi, err := os.Stat(fn); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("trace file mode = %v, err = %v, want 0600", fi.Mode().Perm(), err)
	}

	traceFile = nil
	if err := traceOpts("", filepath.Join(t.TempDir(), "no", "such", "dir", "x.log")); err != nil {
		t.Fatalf("an unopenable trace file must not be fatal: %v", err)
	}
	if traceFile != nil {
		t.Fatal("traceFile set although the open failed")
	}
}
