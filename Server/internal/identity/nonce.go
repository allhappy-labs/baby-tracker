package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"

	"babytracker/server/internal/config"
)

var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func AppleNonce(origin, instanceID, challenge string) (string, error) {
	canonical, err := config.CanonicalOrigin(origin, config.ModeHosted)
	if err != nil || !validNonzeroUUID(instanceID) {
		return "", errors.New("invalid nonce binding")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != challenge {
		return "", errors.New("invalid nonce challenge")
	}
	sum := sha256.Sum256([]byte("baby-tracker-auth-v1\x00" + canonical.String() + "\x00" + instanceID + "\x00" + challenge))
	return hex.EncodeToString(sum[:]), nil
}

func validNonzeroUUID(value string) bool {
	return canonicalUUIDPattern.MatchString(value) && value != "00000000-0000-0000-0000-000000000000"
}
