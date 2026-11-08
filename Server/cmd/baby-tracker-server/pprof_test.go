//go:build !identitytest

package main

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// The release build has no profiling listener: startProfiling is a no-op
// even with the identitytest variable set.
func TestReleaseProfilingIsANoOp(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()
	t.Setenv("BT_IDENTITYTEST_PPROF_ADDRESS", address)
	stop := startProfiling(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if stop == nil {
		t.Fatal("startProfiling returned a nil stop")
	}
	defer stop()
	if connection, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		connection.Close()
		t.Fatalf("the release build listens on %s", address)
	}
}
