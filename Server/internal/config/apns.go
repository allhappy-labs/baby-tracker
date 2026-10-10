package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"regexp"
)

// APNSCredentials are the operator's Apple push credentials (ported from
// main's config/live_activity.go). The key is a mounted file, read at
// startup and never logged; production values are provided at deploy and
// never committed.
type APNSCredentials struct{ TeamID, KeyID, KeyFile, BundleID string }

// APNSWakesConfig turns on background-push wakes (owner decision D4). Off,
// wakes go through the live hub only.
type APNSWakesConfig struct {
	Enabled     bool
	Credentials APNSCredentials
}

var apnsIdentifier = regexp.MustCompile(`^[A-Z0-9]{10}$`)
var apnsBundleID = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+$`)

// LoadAPNSWakes: BT_APNS_WAKES_ENABLED "" or "false" is off (no BT_APNS_*
// variable is read, push stays optional for self-hosters); "true" requires
// LoadAPNSCredentials; anything else is an error.
func LoadAPNSWakes(getenv func(string) string) (APNSWakesConfig, error) {
	if getenv == nil {
		return APNSWakesConfig{}, errors.New("configuration getter is required")
	}
	switch getenv("BT_APNS_WAKES_ENABLED") {
	case "", "false":
		return APNSWakesConfig{}, nil
	case "true":
	default:
		return APNSWakesConfig{}, errors.New("BT_APNS_WAKES_ENABLED must be true or false")
	}
	credentials, err := LoadAPNSCredentials(getenv)
	if err != nil {
		return APNSWakesConfig{}, err
	}
	return APNSWakesConfig{Enabled: true, Credentials: credentials}, nil
}

// LoadAPNSCredentials validates the Apple identifiers (ten upper-case
// alphanumerics), the bundle ID and the mounted key exactly as main does.
// Errors name the variable, never its value.
func LoadAPNSCredentials(getenv func(string) string) (APNSCredentials, error) {
	if getenv == nil {
		return APNSCredentials{}, errors.New("configuration getter is required")
	}
	teamID, keyID, keyFile, bundleID := getenv("BT_APNS_TEAM_ID"), getenv("BT_APNS_KEY_ID"), getenv("BT_APNS_KEY_FILE"), getenv("BT_APNS_BUNDLE_ID")
	if !apnsIdentifier.MatchString(teamID) {
		return APNSCredentials{}, errors.New("BT_APNS_TEAM_ID must be a ten-character Apple team ID")
	}
	if !apnsIdentifier.MatchString(keyID) {
		return APNSCredentials{}, errors.New("BT_APNS_KEY_ID must be a ten-character Apple key ID")
	}
	if !apnsBundleID.MatchString(bundleID) {
		return APNSCredentials{}, errors.New("BT_APNS_BUNDLE_ID must be a bundle identifier")
	}
	if _, err := ReadAPNSPrivateKey(keyFile); err != nil {
		return APNSCredentials{}, err
	}
	return APNSCredentials{TeamID: teamID, KeyID: keyID, KeyFile: keyFile, BundleID: bundleID}, nil
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
