//go:build !identitytest

package main

import "log/slog"

func recoveryCleanupObserved(*slog.Logger) {}
