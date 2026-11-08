//go:build !identitytest

package main

import "log/slog"

// startProfiling is a no-op in the release build: it links no
// net/http/pprof and opens no listener, whatever the environment says.
func startProfiling(*slog.Logger) func() { return func() {} }
