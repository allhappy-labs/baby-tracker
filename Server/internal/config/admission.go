package config

import (
	"errors"
	"time"

	"babytracker/server/internal/admission"
)

// AdmissionConfig limits finite workload requests. These are safety bounds,
// not a measured capacity envelope. MCP remains disabled by default. Wait is
// how long a request may wait for a slot before 503 server_busy
// (BT_ADMISSION_WAIT, 0 through 1s); 0 is no waiting queue.
// AuthMax is the auth lane (BT_ADMISSION_AUTH_MAX, 1 through 1024, default
// admission.DefaultAuth): the requests not yet tied to a known caller, in
// flight alongside Total and never waiting.
type AdmissionConfig struct {
	Total, MCPMax, AuthMax int
	Wait                   time.Duration
}

// DefaultAdmissionWait absorbs the short database stalls the Phase 5 load
// runs met (2.5x passed with it, 2x without), at no cost to a request that
// finds a slot.
const DefaultAdmissionWait = 250 * time.Millisecond

func DefaultAdmission() AdmissionConfig {
	return AdmissionConfig{Total: admission.DefaultTotal, AuthMax: admission.DefaultAuth, Wait: DefaultAdmissionWait}
}

func LoadAdmission(getenv func(string) string) (AdmissionConfig, error) {
	if getenv == nil {
		return AdmissionConfig{}, errors.New("configuration getter is required")
	}
	cfg := DefaultAdmission()
	var err error
	cfg.Total, err = boundedInteger(getenv, "BT_ADMISSION_TOTAL", cfg.Total, 1, 1024)
	if err != nil {
		return AdmissionConfig{}, err
	}
	cfg.MCPMax, err = boundedInteger(getenv, "BT_ADMISSION_MCP_MAX", 0, 0, cfg.Total-1)
	if err != nil {
		return AdmissionConfig{}, err
	}
	cfg.AuthMax, err = boundedInteger(getenv, "BT_ADMISSION_AUTH_MAX", cfg.AuthMax, 1, 1024)
	if err != nil {
		return AdmissionConfig{}, err
	}
	if raw := getenv("BT_ADMISSION_WAIT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 0 || parsed > admission.MaxWait {
			return AdmissionConfig{}, errors.New("BT_ADMISSION_WAIT must be a duration from 0 through 1s")
		}
		cfg.Wait = parsed
	}
	return cfg, nil
}
