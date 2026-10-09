package config

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	defaultAppleRevocationPollInterval = 5 * time.Second
	defaultAppleRevocationLeaseTTL     = 30 * time.Second
	defaultCredentialOrphanGrace       = time.Hour
)

var lifecycleWorkerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type LifecycleConfig struct {
	Enabled      bool
	DatabaseURL  string
	WorkerID     string
	PollInterval time.Duration
	LeaseTTL     time.Duration
	OrphanGrace  time.Duration
}

// LoadLifecycle loads the separately credentialed hosted lifecycle worker.
// Worker settings in non-hosted modes are rejected instead of being silently
// ignored, preventing a stale hosted deployment from appearing self-hosted.
func LoadLifecycle(getenv func(string) string, mode Mode) (LifecycleConfig, error) {
	if getenv == nil {
		return LifecycleConfig{}, errors.New("configuration getter is required")
	}
	settings := []string{
		"BT_APPLE_REVOCATION_DATABASE_URL", "BT_APPLE_REVOCATION_DATABASE_URL_FILE", "BT_APPLE_REVOCATION_WORKER_ID",
		"BT_APPLE_REVOCATION_POLL_INTERVAL", "BT_APPLE_REVOCATION_LEASE_TTL", "BT_CREDENTIAL_ORPHAN_GRACE",
	}
	if mode != ModeHosted {
		for _, name := range settings {
			if getenv(name) != "" {
				return LifecycleConfig{}, fmt.Errorf("%s is available only in hosted mode", name)
			}
		}
		return LifecycleConfig{}, nil
	}
	if getenv("BT_APPLE_REVOCATION_DATABASE_URL") != "" {
		return LifecycleConfig{}, errors.New("BT_APPLE_REVOCATION_DATABASE_URL is forbidden; use BT_APPLE_REVOCATION_DATABASE_URL_FILE")
	}
	databaseURL, err := credentialURL(getenv, "BT_APPLE_REVOCATION_DATABASE_URL", "BT_APPLE_REVOCATION_DATABASE_URL_FILE")
	if err != nil {
		return LifecycleConfig{}, err
	}
	workerID := getenv("BT_APPLE_REVOCATION_WORKER_ID")
	if !lifecycleWorkerIDPattern.MatchString(workerID) {
		return LifecycleConfig{}, errors.New("BT_APPLE_REVOCATION_WORKER_ID must be 1..128 safe opaque characters")
	}
	poll, err := lifecycleDuration(getenv("BT_APPLE_REVOCATION_POLL_INTERVAL"), defaultAppleRevocationPollInterval, time.Second, 5*time.Minute, "BT_APPLE_REVOCATION_POLL_INTERVAL")
	if err != nil {
		return LifecycleConfig{}, err
	}
	lease, err := lifecycleDuration(getenv("BT_APPLE_REVOCATION_LEASE_TTL"), defaultAppleRevocationLeaseTTL, 10*time.Second, 5*time.Minute, "BT_APPLE_REVOCATION_LEASE_TTL")
	if err != nil {
		return LifecycleConfig{}, err
	}
	orphan, err := lifecycleDuration(getenv("BT_CREDENTIAL_ORPHAN_GRACE"), defaultCredentialOrphanGrace, 11*time.Minute, time.Duration(1<<63-1), "BT_CREDENTIAL_ORPHAN_GRACE")
	if err != nil {
		return LifecycleConfig{}, err
	}
	return LifecycleConfig{Enabled: true, DatabaseURL: databaseURL, WorkerID: workerID, PollInterval: poll, LeaseTTL: lease, OrphanGrace: orphan}, nil
}

func lifecycleDuration(raw string, fallback, minimum, maximum time.Duration, name string) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)
	}
	return value, nil
}
