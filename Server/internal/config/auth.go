package config

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

type DigestKeyConfig struct {
	ID     string
	Secret [32]byte
}

type DigestKeyringConfig struct {
	Current  DigestKeyConfig
	Previous []DigestKeyConfig
}

type AuthConfig struct {
	IdentityKeys          DigestKeyringConfig
	SessionTokenKeys      DigestKeyringConfig
	DeviceContinuityKeys  DigestKeyringConfig
	AppleNotificationKeys DigestKeyringConfig
	InvitationKeys        DigestKeyringConfig
	PageTokenKeys         DigestKeyringConfig
	ActorIDPepper         [32]byte
	RateLimitPepper       [32]byte

	// Task 3 compatibility. Composition moves to IdentityKeys in the Task 4 wiring slice.
	IdentityKeyID  string
	IdentityPepper [32]byte
}

var digestKeyIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
var lowercaseHexKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func LoadAuth(getenv func(string) string, mode Mode) (AuthConfig, error) {
	if getenv == nil {
		return AuthConfig{}, errors.New("configuration getter is required")
	}
	if getenv("BT_TEST_ISSUER") != "" {
		return AuthConfig{}, errors.New("BT_TEST_ISSUER is not supported")
	}
	identity, err := loadDigestKeyring(getenv, mode, "BT_IDENTITY", "")
	if err != nil {
		return AuthConfig{}, err
	}
	sessionAlias := getenv("BT_SESSION_SIGNING_KEY_FILE")
	if sessionAlias != "" && getenv("BT_SESSION_TOKEN_PEPPER_FILE") != "" {
		return AuthConfig{}, errors.New("BT_SESSION_SIGNING_KEY_FILE and BT_SESSION_TOKEN_PEPPER_FILE are mutually exclusive")
	}
	session, err := loadDigestKeyring(getenv, mode, "BT_SESSION_TOKEN", sessionAlias)
	if err != nil {
		return AuthConfig{}, err
	}
	device, err := loadDigestKeyring(getenv, mode, "BT_DEVICE_CONTINUITY", "")
	if err != nil {
		return AuthConfig{}, err
	}
	notification, err := loadDigestKeyring(getenv, mode, "BT_APPLE_NOTIFICATION", "")
	if err != nil {
		return AuthConfig{}, err
	}
	invitation, err := loadDigestKeyring(getenv, mode, "BT_INVITATION", "")
	if err != nil {
		return AuthConfig{}, err
	}
	pageToken, err := loadDigestKeyringNamed(getenv, mode, "BT_PAGE_TOKEN", "", "BT_PAGE_TOKEN_KEY_FILE", "BT_PAGE_TOKEN_PREVIOUS_KEYS_FILE", "")
	if err != nil {
		return AuthConfig{}, err
	}
	actor, err := loadPepper(getenv, mode, "BT_ACTOR_ID_PEPPER", "BT_ACTOR_ID_PEPPER_FILE")
	if err != nil {
		return AuthConfig{}, err
	}
	rate, err := loadPepper(getenv, mode, "BT_RATE_LIMIT_PEPPER", "BT_RATE_LIMIT_PEPPER_FILE")
	if err != nil {
		return AuthConfig{}, err
	}

	seenSecrets := make(map[[32]byte]string)
	for _, named := range []struct {
		name string
		ring DigestKeyringConfig
	}{{"identity", identity}, {"session token", session}, {"device continuity", device}, {"Apple notification", notification}, {"invitation", invitation}, {"page token", pageToken}} {
		for _, key := range append([]DigestKeyConfig{named.ring.Current}, named.ring.Previous...) {
			if prior, exists := seenSecrets[key.Secret]; exists {
				return AuthConfig{}, fmt.Errorf("%s key secret duplicates %s", named.name, prior)
			}
			seenSecrets[key.Secret] = named.name
		}
	}
	for _, secret := range []struct {
		name  string
		value [32]byte
	}{{"actor ID", actor}, {"rate-limit", rate}} {
		if prior, exists := seenSecrets[secret.value]; exists {
			return AuthConfig{}, fmt.Errorf("%s pepper duplicates %s", secret.name, prior)
		}
		seenSecrets[secret.value] = secret.name
	}

	return AuthConfig{
		IdentityKeys: identity, SessionTokenKeys: session, DeviceContinuityKeys: device,
		AppleNotificationKeys: notification, InvitationKeys: invitation, PageTokenKeys: pageToken,
		ActorIDPepper: actor, RateLimitPepper: rate,
		IdentityKeyID: identity.Current.ID, IdentityPepper: identity.Current.Secret,
	}, nil
}

func loadDigestKeyring(getenv func(string) string, mode Mode, prefix, fileAlias string) (DigestKeyringConfig, error) {
	return loadDigestKeyringNamed(getenv, mode, prefix, prefix+"_PEPPER", prefix+"_PEPPER_FILE", prefix+"_PREVIOUS_KEYS_FILE", fileAlias)
}

func loadDigestKeyringNamed(getenv func(string) string, mode Mode, prefix, directName, fileName, previousName, fileAlias string) (DigestKeyringConfig, error) {
	keyID := getenv(prefix + "_KEY_ID")
	if !digestKeyIDPattern.MatchString(keyID) {
		return DigestKeyringConfig{}, fmt.Errorf("%s_KEY_ID is invalid", prefix)
	}
	pepperPath := getenv(fileName)
	if fileAlias != "" {
		pepperPath = fileAlias
	}
	current, err := loadPepper(func(name string) string {
		if name == fileName {
			return pepperPath
		}
		return getenv(name)
	}, mode, directName, fileName)
	if err != nil {
		return DigestKeyringConfig{}, err
	}
	ring := DigestKeyringConfig{Current: DigestKeyConfig{ID: keyID, Secret: current}}
	manifestPath := getenv(previousName)
	if manifestPath == "" {
		return ring, nil
	}
	entries, err := loadPreviousManifest(manifestPath)
	if err != nil {
		return DigestKeyringConfig{}, fmt.Errorf("%s_PREVIOUS_KEYS_FILE is invalid", prefix)
	}
	seenIDs := map[string]struct{}{keyID: {}}
	for _, entry := range entries {
		if !digestKeyIDPattern.MatchString(entry.KeyID) || !filepath.IsAbs(entry.PepperFile) {
			return DigestKeyringConfig{}, fmt.Errorf("%s_PREVIOUS_KEYS_FILE is invalid", prefix)
		}
		if _, exists := seenIDs[entry.KeyID]; exists {
			return DigestKeyringConfig{}, fmt.Errorf("%s has duplicate key IDs", prefix)
		}
		seenIDs[entry.KeyID] = struct{}{}
		secret, err := loadPepper(func(name string) string {
			if name == fileName {
				return entry.PepperFile
			}
			return ""
		}, mode, directName, fileName)
		if err != nil {
			return DigestKeyringConfig{}, err
		}
		ring.Previous = append(ring.Previous, DigestKeyConfig{ID: entry.KeyID, Secret: secret})
	}
	return ring, nil
}

type previousKeyEntry struct {
	KeyID      string
	PepperFile string
}

func loadPreviousManifest(path string) ([]previousKeyEntry, error) {
	encoded, err := readRegularFile(path, 16*1024)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return nil, errors.New("expected array")
	}
	entries := make([]previousKeyEntry, 0, 7)
	for decoder.More() {
		if len(entries) == 7 {
			return nil, errors.New("too many previous keys")
		}
		opening, err := decoder.Token()
		if err != nil || opening != json.Delim('{') {
			return nil, errors.New("expected object")
		}
		var entry previousKeyEntry
		seen := map[string]bool{}
		for decoder.More() {
			nameToken, err := decoder.Token()
			name, ok := nameToken.(string)
			if err != nil || !ok || seen[name] || (name != "key_id" && name != "pepper_file") {
				return nil, errors.New("invalid manifest field")
			}
			seen[name] = true
			var value string
			if err := decoder.Decode(&value); err != nil {
				return nil, err
			}
			if name == "key_id" {
				entry.KeyID = value
			} else {
				entry.PepperFile = value
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') || !seen["key_id"] || !seen["pepper_file"] {
			return nil, errors.New("invalid manifest object")
		}
		entries = append(entries, entry)
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim(']') {
		return nil, errors.New("unterminated manifest")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("trailing manifest data")
	}
	return entries, nil
}

func loadPepper(getenv func(string) string, mode Mode, directName, fileName string) ([32]byte, error) {
	var result [32]byte
	direct, path := getenv(directName), getenv(fileName)
	if direct != "" && path != "" {
		return result, fmt.Errorf("%s and %s are mutually exclusive", directName, fileName)
	}
	if mode != ModeDevelopment && direct != "" {
		return result, fmt.Errorf("%s is development-only", directName)
	}
	var encoded []byte
	if path != "" {
		var err error
		encoded, err = readRegularFile(path, 66)
		if err != nil {
			return result, fmt.Errorf("%s could not be read", fileName)
		}
	} else if direct != "" {
		encoded = []byte(direct)
	} else {
		return result, fmt.Errorf("%s is required", fileName)
	}
	if len(encoded) == 65 && encoded[64] == '\n' {
		encoded = encoded[:64]
	} else if len(encoded) == 66 && encoded[64] == '\r' && encoded[65] == '\n' {
		encoded = encoded[:64]
	}
	if !lowercaseHexKeyPattern.Match(encoded) {
		return result, fmt.Errorf("%s must contain 64 lowercase hexadecimal characters", fileName)
	}
	decoded, err := hex.DecodeString(string(encoded))
	if err != nil || len(decoded) != len(result) {
		return result, fmt.Errorf("%s is invalid", fileName)
	}
	copy(result[:], decoded)
	return result, nil
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, openErr := file.Stat()
	pathInfo, pathErr := os.Lstat(path)
	if openErr != nil || pathErr != nil || !openedInfo.Mode().IsRegular() || !pathInfo.Mode().IsRegular() ||
		openedInfo.Size() > limit || !os.SameFile(openedInfo, pathInfo) {
		return nil, errors.New("unsafe secret file")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(encoded)) > limit {
		return nil, errors.New("oversized secret file")
	}
	return encoded, nil
}

// ActorIDPepper loads the actor-ID key alone (BT_ACTOR_ID_PEPPER_FILE, or
// BT_ACTOR_ID_PEPPER in development), for the admin authority commands that
// bind and check its commitment in server_state.
func ActorIDPepper(getenv func(string) string) ([32]byte, error) {
	if getenv == nil {
		return [32]byte{}, errors.New("configuration getter is required")
	}
	mode, err := LoadMode(getenv)
	if err != nil {
		return [32]byte{}, err
	}
	key, err := loadPepper(getenv, mode, "BT_ACTOR_ID_PEPPER", "BT_ACTOR_ID_PEPPER_FILE")
	if err != nil {
		return [32]byte{}, err
	}
	if key == ([32]byte{}) {
		return [32]byte{}, errors.New("BT_ACTOR_ID_PEPPER_FILE must not be all zero")
	}
	return key, nil
}
