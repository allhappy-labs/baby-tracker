//go:build identitytest

package main

import "log/slog"

func recoveryCleanupObserved(logger *slog.Logger) {
	if logger != nil {
		logger.Info("identitytest recovery cleanup completed")
	}
}
