package family

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"

	"babytracker/server/internal/identity"
)

func invitationDigest(key identity.DigestKey, token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		return "", ErrInvitationInvalid
	}
	hash := hmac.New(sha256.New, key.Secret[:])
	for _, field := range [][]byte{[]byte("baby-tracker-invitation-token-v1"), raw} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(field)))
		hash.Write(size[:])
		hash.Write(field)
	}
	return "hmac-sha256:" + key.ID + ":" + hex.EncodeToString(hash.Sum(nil)), nil
}

func invitationCandidates(keys identity.DigestKeyring, token string) ([]string, error) {
	result := make([]string, 0, 1+len(keys.Previous))
	current, err := invitationDigest(keys.Current, token)
	if err != nil {
		return nil, err
	}
	result = append(result, current)
	for _, key := range keys.Previous {
		digest, err := invitationDigest(key, token)
		if err != nil {
			return nil, err
		}
		result = append(result, digest)
	}
	return result, nil
}

func newInvitationToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validInvitationToken(value string) bool {
	if len(value) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}
