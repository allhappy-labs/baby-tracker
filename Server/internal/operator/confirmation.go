package operator

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"time"
)

type confirmationEnvelope struct {
	Version    string          `json:"version"`
	Operation  string          `json:"operation"`
	OperatorID string          `json:"operator_id"`
	Proposal   json.RawMessage `json:"proposal"`
	ExpiresAt  string          `json:"expires_at"`
}

func confirmationDigest(key []byte, operation, operatorID string, canonicalProposal []byte, expires time.Time) (string, error) {
	if len(key) != 32 || !validUUID(operatorID) || !json.Valid(canonicalProposal) || expires.Location() != time.UTC || expires.Nanosecond() != 0 {
		return "", ErrInvalidConfirmation
	}
	envelope, err := json.Marshal(confirmationEnvelope{Version: "v1", Operation: operation, OperatorID: operatorID,
		Proposal: append(json.RawMessage(nil), canonicalProposal...), ExpiresAt: expires.Format(time.RFC3339)})
	if err != nil {
		return "", ErrInvalidConfirmation
	}
	digest := hmac.New(sha256.New, key)
	digest.Write([]byte("baby-tracker-operator-confirmation-v1\x00"))
	digest.Write(envelope)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func evidenceDigest(key []byte, reference string) string {
	digest := hmac.New(sha256.New, key)
	digest.Write([]byte("baby-tracker-operator-evidence-v1\x00"))
	digest.Write([]byte(reference))
	return hex.EncodeToString(digest.Sum(nil))
}

func equalHexDigest(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	return leftErr == nil && rightErr == nil && len(leftBytes) == sha256.Size && len(rightBytes) == sha256.Size &&
		subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}
