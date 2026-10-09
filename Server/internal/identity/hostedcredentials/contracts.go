package hostedcredentials

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"time"

	"babytracker/server/internal/identity"
	"babytracker/server/internal/security/hosted"
)

const (
	credentialEnvelopeSchema = "credential-envelope-v1"
	credentialAADDomain      = "baby-tracker-credential-envelope-aad-v1"
	maxJSONSafeInteger       = uint64(9007199254740991)
)

var credentialReference = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type CredentialEnvelopeHeader struct {
	Schema           string
	EnvelopeSequence uint64
}

type SecretObject struct {
	ID         string
	Version    string
	KeyID      string
	Header     CredentialEnvelopeHeader
	Ciphertext []byte
	SHA256     [32]byte
}

type SecretMetadata struct {
	ObjectID  string
	Version   string
	KeyID     string
	CreatedAt time.Time
}

type SecretStore interface {
	Resolve(context.Context, string) (SecretObject, bool, error)
	Create(context.Context, SecretObject) error
	CompareAndSwap(context.Context, string, string, SecretObject) (bool, error)
	Delete(context.Context, string, string) error
	ListMetadata(context.Context, time.Time, string, int) ([]SecretMetadata, string, error)
}

type CredentialReplayMarkerPurger interface {
	PurgeReplayMarker(context.Context, identity.CredentialBinding) error
}

type CredentialAdapters struct {
	Vault         identity.CredentialVault
	Inventory     identity.CredentialInventory
	ReplayMarkers CredentialReplayMarkerPurger
}

func NewCredentialAdapters(store SecretStore, cipher hosted.EnvelopeCipher, keyID string) (CredentialAdapters, error) {
	return newCredentialAdapters(store, cipher, keyID, time.Now)
}

func replayObjectID(binding identity.CredentialBinding) (string, error) {
	expected, err := identity.NewAppleCredentialBinding(binding.AccountID, binding.ServerInstanceID)
	if err != nil || expected != binding {
		return "", identity.ErrCredentialConflict
	}
	digest := sha256.Sum256([]byte("baby-tracker-credential-replay-object-v1\x00" + binding.ServerInstanceID + "\x00" + binding.AccountID + "\x00" + binding.Purpose))
	raw := append([]byte(nil), digest[:16]...)
	raw[6] = raw[6]&0x0f | 0x80
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func credentialAAD(objectID, keyID string, header CredentialEnvelopeHeader) ([]byte, error) {
	if !credentialReference.MatchString(objectID) || !credentialReference.MatchString(keyID) ||
		header.Schema != credentialEnvelopeSchema || header.EnvelopeSequence == 0 || header.EnvelopeSequence > maxJSONSafeInteger {
		return nil, identity.ErrCredentialConflict
	}
	var sequence [8]byte
	binary.BigEndian.PutUint64(sequence[:], header.EnvelopeSequence)
	return frame([]byte(credentialAADDomain), []byte(objectID), []byte(keyID), []byte(header.Schema), sequence[:]), nil
}

func frame(fields ...[]byte) []byte {
	total := 0
	for _, field := range fields {
		total += 4 + len(field)
	}
	result := make([]byte, 0, total)
	var length [4]byte
	for _, field := range fields {
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		result = append(result, length[:]...)
		result = append(result, field...)
	}
	return result
}
