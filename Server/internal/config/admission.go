package config

import (
	"errors"

	"babytracker/server/internal/admission"
)

// AdmissionConfig limits finite workload requests. These are safety bounds,
// not a measured capacity envelope. MCP remains disabled by default.
type AdmissionConfig struct{ Total, MCPMax int }

func DefaultAdmission() AdmissionConfig { return AdmissionConfig{Total: admission.DefaultTotal} }

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
	return cfg, nil
}
