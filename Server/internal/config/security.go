package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"babytracker/server/internal/security/hosted"
)

type SecurityBackend string

const (
	SecurityBackendFilesystem SecurityBackend = "filesystem"
	SecurityBackendHosted     SecurityBackend = "hosted_provider"
)

type SecurityConfig struct {
	Backend   SecurityBackend
	LedgerDir string
	HeadDir   string
	Hosted    HostedSecurityConfig
}

type HostedSecurityConfig struct {
	ProviderID            string
	Region                string
	ApprovedRegions       []string
	LedgerFailureDomain   string
	HeadFailureDomain     string
	LedgerKeyID           string
	HeadKeyID             string
	VaultKeyID            string
	ReplayScratchPath     string
	ReplayScratchMaxBytes int64
}

var hostedSecuritySettingNames = []string{
	"BT_HOSTED_PROVIDER_ID",
	"BT_HOSTED_REGION",
	"BT_HOSTED_APPROVED_REGIONS",
	"BT_HOSTED_LEDGER_FAILURE_DOMAIN",
	"BT_HOSTED_HEAD_FAILURE_DOMAIN",
	"BT_HOSTED_LEDGER_KEY_ID",
	"BT_HOSTED_HEAD_KEY_ID",
	"BT_HOSTED_VAULT_KEY_ID",
	"BT_SECURITY_REPLAY_SCRATCH_PATH",
	"BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES",
}

// LoadSecurity makes the storage choice mode-bound. Hosted mode has no local
// path fallback: it records only the policy a separately registered concrete
// provider must satisfy before the server may compose authority services.
func LoadSecurity(getenv func(string) string, mode Mode) (SecurityConfig, error) {
	if getenv == nil {
		return SecurityConfig{}, errors.New("configuration getter is required")
	}
	if mode != ModeHosted {
		for _, name := range hostedSecuritySettingNames {
			if getenv(name) != "" {
				return SecurityConfig{}, fmt.Errorf("%s is available only in hosted mode", name)
			}
		}
		ledgerDir, err := absoluteDir("BT_SECURITY_LEDGER_DIR", getenv("BT_SECURITY_LEDGER_DIR"))
		if err != nil {
			return SecurityConfig{}, err
		}
		headDir, err := absoluteDir("BT_LEDGER_HEAD_DIR", getenv("BT_LEDGER_HEAD_DIR"))
		if err != nil {
			return SecurityConfig{}, err
		}
		if ledgerDir == headDir {
			return SecurityConfig{}, errors.New("BT_SECURITY_LEDGER_DIR and BT_LEDGER_HEAD_DIR must differ")
		}
		return SecurityConfig{Backend: SecurityBackendFilesystem, LedgerDir: ledgerDir, HeadDir: headDir}, nil
	}

	if getenv("BT_SECURITY_LEDGER_DIR") != "" || getenv("BT_LEDGER_HEAD_DIR") != "" {
		return SecurityConfig{}, errors.New("hosted mode does not accept filesystem security stores")
	}
	configuration := HostedSecurityConfig{
		ProviderID:          getenv("BT_HOSTED_PROVIDER_ID"),
		Region:              getenv("BT_HOSTED_REGION"),
		LedgerFailureDomain: getenv("BT_HOSTED_LEDGER_FAILURE_DOMAIN"),
		HeadFailureDomain:   getenv("BT_HOSTED_HEAD_FAILURE_DOMAIN"),
		LedgerKeyID:         getenv("BT_HOSTED_LEDGER_KEY_ID"),
		HeadKeyID:           getenv("BT_HOSTED_HEAD_KEY_ID"),
		VaultKeyID:          getenv("BT_HOSTED_VAULT_KEY_ID"),
	}
	regions, err := parseHostedIdentifiers(getenv("BT_HOSTED_APPROVED_REGIONS"))
	if err != nil {
		return SecurityConfig{}, errors.New("BT_HOSTED_APPROVED_REGIONS is invalid")
	}
	configuration.ApprovedRegions = regions
	scratchPath, err := absoluteDir("BT_SECURITY_REPLAY_SCRATCH_PATH", getenv("BT_SECURITY_REPLAY_SCRATCH_PATH"))
	if err != nil || scratchPath == string(filepath.Separator) {
		return SecurityConfig{}, errors.New("BT_SECURITY_REPLAY_SCRATCH_PATH must be an absolute non-root path")
	}
	configuration.ReplayScratchPath = scratchPath
	maxBytes, err := strconv.ParseInt(getenv("BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES"), 10, 64)
	if err != nil || maxBytes < 48 || maxBytes > 1<<30 {
		return SecurityConfig{}, errors.New("BT_SECURITY_REPLAY_SCRATCH_MAX_BYTES must be an integer from 48 through 1073741824")
	}
	configuration.ReplayScratchMaxBytes = maxBytes
	if err := configuration.validatePolicy(); err != nil {
		return SecurityConfig{}, err
	}
	return SecurityConfig{Backend: SecurityBackendHosted, Hosted: configuration}, nil
}

// ValidateObserved binds a provider's runtime assertions to the exact operator
// policy selected at startup. The booleans still require live provider
// acceptance; configuration alone cannot assert them.
func (configuration HostedSecurityConfig) ValidateObserved(observed hosted.Properties) error {
	if observed.Provider != configuration.ProviderID || observed.Region != configuration.Region ||
		observed.LedgerFailureDomain != configuration.LedgerFailureDomain ||
		observed.HeadFailureDomain != configuration.HeadFailureDomain {
		return errors.New("hosted store properties do not match configured policy")
	}
	return hosted.ValidateProperties(observed, hosted.PropertyPolicy{
		AllowedProviders: []string{configuration.ProviderID},
		AllowedRegions:   append([]string(nil), configuration.ApprovedRegions...),
		LedgerKeyID:      configuration.LedgerKeyID,
		HeadKeyID:        configuration.HeadKeyID,
	})
}

func (configuration HostedSecurityConfig) validatePolicy() error {
	for _, value := range []string{
		configuration.ProviderID, configuration.Region, configuration.LedgerFailureDomain,
		configuration.HeadFailureDomain, configuration.LedgerKeyID, configuration.HeadKeyID, configuration.VaultKeyID,
	} {
		if !validHostedIdentifier(value) {
			return errors.New("hosted security policy contains an invalid identifier")
		}
	}
	if configuration.VaultKeyID == configuration.LedgerKeyID || configuration.VaultKeyID == configuration.HeadKeyID {
		return errors.New("hosted ledger, head, and vault key IDs must differ")
	}
	return configuration.ValidateObserved(hosted.Properties{
		Provider: configuration.ProviderID, Region: configuration.Region,
		LedgerFailureDomain: configuration.LedgerFailureDomain, HeadFailureDomain: configuration.HeadFailureDomain,
		LedgerKeyID: configuration.LedgerKeyID, HeadKeyID: configuration.HeadKeyID,
		Encrypted: true, ImmutableLedger: true, ReplicatedHead: true, ConditionalCreate: true, LinearizableCAS: true,
	})
}

func parseHostedIdentifiers(raw string) ([]string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, errors.New("invalid identifiers")
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	for _, value := range parts {
		if strings.TrimSpace(value) != value || !validHostedIdentifier(value) {
			return nil, errors.New("invalid identifiers")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("duplicate identifier")
		}
		seen[value] = struct{}{}
	}
	return parts, nil
}

func validHostedIdentifier(value string) bool {
	if len(value) < 1 || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
