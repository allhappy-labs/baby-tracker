package config

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"babytracker/server/internal/limits"
	"babytracker/server/internal/protocol"
)

type Mode string

const (
	ModeDevelopment Mode = "development"
	ModeHosted      Mode = "hosted"
	ModeSelfHosted  Mode = "self_hosted"
)

type Config struct {
	Admission         AdmissionConfig
	DatabasePools     DatabasePools
	Mode              Mode
	ListenAddress     string
	DatabaseURL       string
	PublicOrigin      *url.URL
	ServerInstanceID  string
	RecoveryEpoch     uint64
	AppleAudience     string
	TrustedProxyCIDRs []netip.Prefix
	Auth              AuthConfig
	Limits            LimitsConfig
	Sync              SyncConfig
	Export            ExportConfig
	APNSWakes         APNSWakesConfig
	LiveActivity      LiveActivityConfig
	RecoveryCleanup   RecoveryCleanupConfig
	MCP               MCPConfig
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("configuration getter is required")
	}
	var errs []error
	if err := RejectRetiredSecuritySettings(getenv); err != nil {
		errs = append(errs, err)
	}
	mode, modeErr := LoadMode(getenv)
	if modeErr != nil {
		errs = append(errs, modeErr)
	}
	listen := getenv("BT_LISTEN_ADDRESS")
	if listen == "" {
		listen = ":8080"
	}
	databaseURL, databaseErr := DatabaseURL(getenv)
	if databaseErr != nil {
		errs = append(errs, databaseErr)
	}
	origin, err := CanonicalOrigin(getenv("BT_PUBLIC_ORIGIN"), mode)
	if err != nil {
		errs = append(errs, fmt.Errorf("BT_PUBLIC_ORIGIN: %w", err))
	}
	instanceID := getenv("BT_SERVER_INSTANCE_ID")
	if !uuidPattern.MatchString(instanceID) {
		errs = append(errs, errors.New("BT_SERVER_INSTANCE_ID must be a lowercase hyphenated UUID"))
	}
	epochText := getenv("BT_RECOVERY_EPOCH")
	epoch, epochErr := strconv.ParseUint(epochText, 10, 64)
	if epochErr != nil || !protocol.ValidRecoveryEpoch(epoch) {
		errs = append(errs, errors.New("BT_RECOVERY_EPOCH must be an integer from 1 through 9007199254740991"))
	}
	audience := getenv("BT_APPLE_AUDIENCE")
	if audience == "" {
		errs = append(errs, errors.New("BT_APPLE_AUDIENCE is required"))
	}
	proxies, proxyErr := parseCIDRs(getenv("BT_TRUSTED_PROXY_CIDRS"))
	if proxyErr != nil {
		errs = append(errs, proxyErr)
	}
	auth, authErr := LoadAuth(getenv, mode)
	if authErr != nil {
		errs = append(errs, authErr)
	}
	limitConfig, limitErr := LoadLimits(getenv)
	if limitErr != nil {
		errs = append(errs, limitErr)
	}
	syncConfig, syncErr := LoadSync(getenv)
	if syncErr != nil {
		errs = append(errs, syncErr)
	} else if limitErr == nil {
		syncConfig.Limits.SyncMutationDevicePerMinute = limitConfig.Policies[limits.ScopeSyncMutationDevice].Count
		syncConfig.Limits.SyncMutationAccountPerMinute = limitConfig.Policies[limits.ScopeSyncMutationAccount].Count
		syncConfig.Limits.SyncMutationFamilyPerMinute = limitConfig.Policies[limits.ScopeSyncMutationFamily].Count
		syncConfig.Limits.SyncPullDevicePerMinute = limitConfig.Policies[limits.ScopeSyncPullDevice].Count
		syncConfig.Limits.SyncPullAccountPerMinute = limitConfig.Policies[limits.ScopeSyncPullAccount].Count
		syncConfig.Limits.SocketAttemptIPPerMinute = limitConfig.Policies[limits.ScopeLiveAttemptIP].Count
		syncConfig.Limits.SocketAttemptDevicePerMinute = limitConfig.Policies[limits.ScopeLiveAttemptDevice].Count
		syncConfig.Limits.SocketAttemptAccountPerMinute = limitConfig.Policies[limits.ScopeLiveAttemptAccount].Count
		syncConfig.Limits.SocketAttemptFamilyPerMinute = limitConfig.Policies[limits.ScopeLiveAttemptFamily].Count
	}
	exportConfig, exportErr := LoadExport(getenv)
	if exportErr != nil {
		errs = append(errs, exportErr)
	} else if exportConfig.Enabled && exportConfig.WorkerDatabaseURL == databaseURL {
		errs = append(errs, errors.New("export worker database credential must differ from the runtime credential"))
	}
	apnsWakes, apnsErr := LoadAPNSWakes(getenv)
	if apnsErr != nil {
		errs = append(errs, apnsErr)
	}
	liveActivity, liveActivityErr := LoadLiveActivity(getenv)
	// Both switches load the same BT_APNS_* credentials: a credential error
	// with both on is reported once.
	if liveActivityErr != nil && (apnsErr == nil || liveActivityErr.Error() != apnsErr.Error()) {
		errs = append(errs, liveActivityErr)
	}
	recoveryCleanup, cleanupErr := LoadRecoveryCleanup(getenv, exportConfig)
	if cleanupErr != nil {
		errs = append(errs, cleanupErr)
	} else if !recoveryCleanup.Enabled && exportErr == nil {
		// Decision D3: the maintenance credential is required in every mode.
		errs = append(errs, errors.New("BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE (or BT_EXPORT_DATABASE_URL_FILE) is required: retention, the family purge and wakes run as the maintenance principal"))
	} else if recoveryCleanup.Enabled && recoveryCleanup.WorkerDatabaseURL == databaseURL {
		errs = append(errs, errors.New("recovery maintenance credential must differ from the runtime credential"))
	}
	admissionConfig, admissionErr := LoadAdmission(getenv)
	if admissionErr != nil {
		errs = append(errs, admissionErr)
	}
	databasePools, poolsErr := LoadDatabasePools(getenv)
	if poolsErr != nil {
		errs = append(errs, poolsErr)
	} else if databasePools.Explicit() && databasePools.Maintenance < 2 {
		errs = append(errs, errors.New("BT_DB_MAINTENANCE_MAX_CONNS must be at least 2: one maintenance connection is reserved for the readiness audit beside the workers"))
	}
	mcpConfig, mcpErr := LoadMCP(getenv)
	if mcpErr != nil {
		errs = append(errs, mcpErr)
	} else if admissionErr == nil && poolsErr == nil && modeErr == nil {
		if err := validateMCPComposition(mcpConfig, mode, admissionConfig, databasePools); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return Config{MCP: mcpConfig, Admission: admissionConfig, DatabasePools: databasePools, Mode: mode, ListenAddress: listen, DatabaseURL: databaseURL, PublicOrigin: origin, ServerInstanceID: instanceID, RecoveryEpoch: epoch, AppleAudience: audience, TrustedProxyCIDRs: proxies, Auth: auth, Limits: limitConfig, Sync: syncConfig, Export: exportConfig, APNSWakes: apnsWakes, LiveActivity: liveActivity, RecoveryCleanup: recoveryCleanup}, nil
}

// LoadMode is shared by server and administrative composition so a dangerous
// command runs under the same mode (and entitlement policy) as the server.
func LoadMode(getenv func(string) string) (Mode, error) {
	if getenv == nil {
		return "", errors.New("configuration getter is required")
	}
	mode := Mode(getenv("BT_MODE"))
	if mode == "" {
		mode = ModeDevelopment
	}
	if mode != ModeDevelopment && mode != ModeHosted && mode != ModeSelfHosted {
		return "", errors.New("BT_MODE must be development, hosted, or self_hosted")
	}
	return mode, nil
}

// DatabaseURL is shared by the server and migration command. Errors deliberately
// omit both the file path and credential contents. The size limit includes any
// trailing line ending; reads are bounded even if the file changes after Stat.
func DatabaseURL(getenv func(string) string) (string, error) {
	return credentialURL(getenv, "BT_DATABASE_URL", "BT_DATABASE_URL_FILE")
}

func credentialURL(getenv func(string) string, valueName, fileName string) (string, error) {
	if getenv == nil {
		return "", errors.New("configuration getter is required")
	}
	value, path := getenv(valueName), getenv(fileName)
	if value != "" && path != "" {
		return "", fmt.Errorf("%s and %s are mutually exclusive", valueName, fileName)
	}
	if path == "" {
		if value == "" {
			return "", fmt.Errorf("%s or %s is required", valueName, fileName)
		}
		return value, nil
	}
	readError := fmt.Errorf("%s could not be read", fileName)
	contentError := fmt.Errorf("%s must contain one nonempty line of at most 16 KiB", fileName)
	file, err := os.Open(path)
	if err != nil {
		return "", readError
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", readError
	}
	if info.Size() > 16<<10 {
		return "", contentError
	}
	data, err := io.ReadAll(io.LimitReader(file, 16<<10))
	if err != nil {
		return "", readError
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return "", readError
	}
	if finalInfo.Size() > 16<<10 {
		return "", contentError
	}
	value = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	// A CR is accepted only as part of the one trailing CRLF.
	if strings.HasSuffix(string(data), "\r") || value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", contentError
	}
	return value, nil
}

func CanonicalOrigin(raw string, mode Mode) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, `\`) {
		return nil, errors.New("is required")
	}
	for _, character := range []byte(raw) {
		if character > 0x7f {
			return nil, errors.New("must use an ASCII DNS host")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return nil, errors.New("must be an absolute URL")
	}
	if mode != ModeDevelopment && mode != ModeHosted && mode != ModeSelfHosted {
		return nil, errors.New("has an unsupported mode")
	}
	if mode != ModeDevelopment && !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("must use HTTPS outside development")
	}
	if mode == ModeDevelopment && !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, errors.New("must use HTTP or HTTPS in development")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("must not include path, credentials, query, or fragment")
	}
	hostname := u.Hostname()
	if hostname == "" || strings.HasSuffix(hostname, ".") || len(hostname) > 253 {
		return nil, errors.New("must include a host")
	}
	if _, ipErr := netip.ParseAddr(hostname); ipErr == nil {
		return nil, errors.New("must use a DNS host")
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return nil, errors.New("must use a canonical DNS host")
		}
		for index := range label {
			character := label[index]
			if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') || character == '-') {
				return nil, errors.New("must use a canonical DNS host")
			}
		}
	}
	port := u.Port()
	var parsedPort uint64
	if port != "" {
		var parseErr error
		parsedPort, parseErr = strconv.ParseUint(port, 10, 16)
		if parseErr != nil || parsedPort == 0 {
			return nil, errors.New("must use a valid port")
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(hostname)
	if port != "" && !(u.Scheme == "https" && parsedPort == 443) {
		u.Host += ":" + strconv.FormatUint(parsedPort, 10)
	}
	u.Path, u.RawPath = "", ""
	return u, nil
}

func parseCIDRs(raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("BT_TRUSTED_PROXY_CIDRS is required")
	}
	parts := strings.Split(raw, ",")
	result := make([]netip.Prefix, 0, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			return nil, errors.New("BT_TRUSTED_PROXY_CIDRS contains an invalid CIDR")
		}
		result = append(result, prefix)
	}
	return result, nil
}
