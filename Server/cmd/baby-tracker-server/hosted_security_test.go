//go:build !identitytest

package main

import (
	"errors"
	"testing"

	"babytracker/server/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeServicesHostedCannotOpenFilesystemFallback(t *testing.T) {
	t.Setenv("BT_IDENTITYTEST_HOSTED_ADAPTER", "1")
	cfg := config.Config{
		Mode: config.ModeHosted,
		Security: config.SecurityConfig{
			Backend: config.SecurityBackendHosted,
		},
	}
	runtimePool, projectorPool := &pgxpool.Pool{}, &pgxpool.Pool{}
	_, _, _, err := runtimeServices(cfg, runtimePool, projectorPool)
	if !errors.Is(err, errHostedAdapterUnavailable) {
		t.Fatalf("hosted runtime composition error=%v", err)
	}
}
