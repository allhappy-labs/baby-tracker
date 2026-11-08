package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"babytracker/server/internal/protocol"
)

// DefaultBodyReadTimeout bounds one unauthenticated request-body read (the
// sync, import and push-token routes read their body before the credential
// check): 1 MiB in 5 s is about 1.7 Mbit/s, and a phone's bodies are a few
// kilobytes.
const DefaultBodyReadTimeout = 5 * time.Second

type SyncConfig struct {
	Limits protocol.Limits
	// BodyReadTimeout is BT_BODY_READ_TIMEOUT, a Go duration from 1s through
	// 60s (default 5s).
	BodyReadTimeout time.Duration
}

func LoadSync(getenv func(string) string) (SyncConfig, error) {
	if getenv == nil {
		return SyncConfig{}, errors.New("configuration getter is required")
	}
	limits := protocol.DefaultLimits()
	for _, value := range []struct {
		name             string
		destination      *int64
		minimum, maximum int64
	}{
		{"BT_MAX_CANONICAL_RECORDS_PER_FAMILY", &limits.MaxCanonicalRecordsPerFamily, 100, 1000000},
		{"BT_MAX_CHARGED_STORAGE_BYTES_PER_FAMILY", &limits.MaxChargedStorageBytesPerFamily, 16 << 20, int64(4) << 30},
	} {
		raw := getenv(value.name)
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || strconv.FormatInt(parsed, 10) != raw || parsed < value.minimum || parsed > value.maximum {
			return SyncConfig{}, fmt.Errorf("%s must be a canonical integer from %d through %d", value.name, value.minimum, value.maximum)
		}
		*value.destination = parsed
	}
	values := []struct {
		name             string
		destination      *int
		minimum, maximum int
	}{
		{"BT_MAX_LIVE_BABIES", &limits.MaxLiveBabies, 1, 100},
		{"BT_SYNC_BODY_BYTES", &limits.SyncBodyBytes, 65536, 4194304},
		{"BT_SYNC_BATCH_SIZE", &limits.SyncBatchSize, 1, 200},
		{"BT_SYNC_PAGE_SIZE", &limits.SyncPageSize, 1, 1000},
		{"BT_SYNC_PAGE_BYTES", &limits.SyncPageBytes, 64 << 10, 4 << 20},
		{"BT_SYNC_MUTATION_BYTES", &limits.SyncMutationBytes, 4096, 65536},
		{"BT_SYNC_PAGE_TTL_SECONDS", &limits.SyncPageTTLSeconds, 60, 3600},
		{"BT_SNAPSHOT_TTL_SECONDS", &limits.SnapshotTTLSeconds, 60, 3600},
		{"BT_SNAPSHOT_MAX_ENTITIES", &limits.SnapshotMaxEntities, 1000, 2000000},
		{"BT_SNAPSHOT_MAX_BYTES", &limits.SnapshotMaxBytes, 1 << 20, 512 << 20},
		{"BT_SYNC_TRANSACTION_SECONDS", &limits.SyncTransactionSeconds, 1, 30},
		{"BT_IMPORT_MAX_RECORDS", &limits.ImportMaxRecords, 100, 1000000},
		{"BT_IMPORT_MAX_BABIES", &limits.ImportMaxBabies, 1, 500},
		{"BT_IMPORT_STAGING_TTL_HOURS", &limits.ImportStagingTTLHours, 1, 720},
		{"BT_IMPORT_MAX_SESSIONS_PER_FAMILY", &limits.ImportMaxSessionsPerFamily, 10, 10000},
		{"BT_IMPORT_MAX_SESSIONS_PER_ACCOUNT", &limits.ImportMaxSessionsPerAccount, 1, 1000},
		{"BT_SOCKET_DEVICE_LIMIT", &limits.SocketDeviceLimit, 1, 5},
		{"BT_SOCKET_ACCOUNT_LIMIT", &limits.SocketAccountLimit, 1, 10},
		{"BT_SOCKET_PROCESS_LIMIT", &limits.SocketProcessLimit, 10, protocol.MaxSocketProcessLimit},
	}
	for _, value := range values {
		parsed, err := boundedInteger(getenv, value.name, *value.destination, value.minimum, value.maximum)
		if err != nil {
			return SyncConfig{}, err
		}
		*value.destination = parsed
	}
	if err := limits.Validate(); err != nil {
		return SyncConfig{}, fmt.Errorf("sync limits: %w", err)
	}
	bodyReadTimeout := DefaultBodyReadTimeout
	if raw := getenv("BT_BODY_READ_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < time.Second || parsed > time.Minute {
			return SyncConfig{}, errors.New("BT_BODY_READ_TIMEOUT must be a duration from 1s through 60s")
		}
		bodyReadTimeout = parsed
	}
	return SyncConfig{Limits: limits, BodyReadTimeout: bodyReadTimeout}, nil
}
