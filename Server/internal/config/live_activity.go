package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"regexp"
	"strings"
)

type LiveActivityConfig struct {
	Enabled           bool
	TeamID            string
	KeyID             string
	KeyFile           string
	BundleID          string
	WorkerDatabaseURL string
}

var apnsIdentifier = regexp.MustCompile(`^[A-Z0-9]{10}$`)
var apnsBundleID = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

// LoadLiveActivity is optional in every server mode. Disabled or absent
// credentials never affect ordinary family sync.
func LoadLiveActivity(getenv func(string) string) (LiveActivityConfig, error) {
	if getenv == nil {
		return LiveActivityConfig{}, errors.New("configuration getter is required")
	}
	switch getenv("BT_LIVE_ACTIVITY_PUSH_ENABLED") {
	case "", "false":
		return LiveActivityConfig{}, nil
	case "true":
	default:
		return LiveActivityConfig{}, errors.New("BT_LIVE_ACTIVITY_PUSH_ENABLED must be true or false")
	}
	configuration, err := LoadAPNSCredentials(getenv)
	if err != nil {
		return LiveActivityConfig{}, err
	}
	if getenv("BT_LIVE_ACTIVITY_DATABASE_URL") != "" {
		return LiveActivityConfig{}, ErrLiveActivityInlineDatabaseURL
	}
	configuration.WorkerDatabaseURL, err = ReadLiveActivityDatabaseURL(getenv("BT_LIVE_ACTIVITY_DATABASE_URL_FILE"))
	if err != nil {
		return LiveActivityConfig{}, err
	}
	return configuration, nil
}

// ErrLiveActivityInlineDatabaseURL rejects a worker URL passed in the
// environment; only the mounted file form is accepted.
var ErrLiveActivityInlineDatabaseURL = errors.New("BT_LIVE_ACTIVITY_DATABASE_URL is forbidden; use BT_LIVE_ACTIVITY_DATABASE_URL_FILE")

// LoadAPNSCredentials validates the Apple identifiers and the mounted signing
// key exactly as an enabled startup does, independently of the worker database
// URL. The result has Enabled set and no WorkerDatabaseURL; it is what
// LoadLiveActivity builds on and what the preflight command checks before any
// worker credential exists.
func LoadAPNSCredentials(getenv func(string) string) (LiveActivityConfig, error) {
	if getenv == nil {
		return LiveActivityConfig{}, errors.New("configuration getter is required")
	}
	teamID, keyID, keyFile, bundleID := getenv("BT_APNS_TEAM_ID"), getenv("BT_APNS_KEY_ID"), getenv("BT_APNS_KEY_FILE"), getenv("BT_APNS_BUNDLE_ID")
	if !apnsIdentifier.MatchString(teamID) {
		return LiveActivityConfig{}, errors.New("BT_APNS_TEAM_ID must be a ten-character Apple team ID")
	}
	if !apnsIdentifier.MatchString(keyID) {
		return LiveActivityConfig{}, errors.New("BT_APNS_KEY_ID must be a ten-character Apple key ID")
	}
	if !apnsBundleID.MatchString(bundleID) {
		return LiveActivityConfig{}, errors.New("BT_APNS_BUNDLE_ID must be a bundle identifier")
	}
	if _, err := ReadAPNSPrivateKey(keyFile); err != nil {
		return LiveActivityConfig{}, err
	}
	return LiveActivityConfig{Enabled: true, TeamID: teamID, KeyID: keyID, KeyFile: keyFile, BundleID: bundleID}, nil
}

// ReadLiveActivityDatabaseURL reads the worker's URL file under the startup
// rules: a regular nonsymlink file, mode exactly 0600, one nonempty line.
func ReadLiveActivityDatabaseURL(path string) (string, error) {
	invalid := errors.New("BT_LIVE_ACTIVITY_DATABASE_URL_FILE must be a regular nonsymlink file with mode 0600 and one nonempty line of at most 16 KiB")
	if path == "" {
		return "", invalid
	}
	before, err := os.Lstat(path)
	if err != nil || !exactOperatorSecretMode(before.Mode()) {
		return "", invalid
	}
	data, err := readRegularFile(path, 16<<10)
	if err != nil {
		return "", invalid
	}
	after, err := os.Lstat(path)
	if err != nil || !exactOperatorSecretMode(after.Mode()) || !os.SameFile(before, after) {
		return "", invalid
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.HasSuffix(string(data), "\r") || value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", invalid
	}
	return value, nil
}

// ReadAPNSPrivateKey loads a mounted P-256 signing key once at startup or
// explicit configuration reload, never for each notification.
func ReadAPNSPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	invalid := errors.New("BT_APNS_KEY_FILE must be a regular nonsymlink P-256 PEM file with mode 0600")
	if path == "" {
		return nil, invalid
	}
	info, err := os.Lstat(path)
	if err != nil || !exactOperatorSecretMode(info.Mode()) {
		return nil, invalid
	}
	raw, err := readRegularFile(path, 16<<10)
	if err != nil {
		return nil, invalid
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(rest) != 0 || len(block.Headers) != 0 {
		return nil, invalid
	}
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, invalid
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		return nil, invalid
	}
	return key, nil
}
