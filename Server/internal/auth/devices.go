package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"

	"babytracker/server/internal/authority"
	"babytracker/server/internal/identity"
)

func deviceCredentialDigest(key identity.DigestKey, origin, instanceID, accountID, deviceID, credential string) identity.KeyedDigest {
	digest := hmac.New(sha256.New, key.Secret[:])
	digest.Write([]byte("baby-tracker-device-continuity-v1\x00" + origin + "\x00" + instanceID + "\x00" + accountID + "\x00" + deviceID + "\x00" + credential))
	var sum [32]byte
	copy(sum[:], digest.Sum(nil))
	return identity.KeyedDigest{KeyID: key.ID, Sum: sum}
}

// newDevice mints a device for accountID and its one-time continuity
// credential; the binding is registered by the sign-in that carries it.
func (service *Service) newDevice(accountID string) (authority.DeviceBinding, string, error) {
	deviceID, err := newUUID()
	if err != nil {
		return authority.DeviceBinding{}, "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	credential, err := newToken()
	if err != nil {
		return authority.DeviceBinding{}, "", fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	digest := deviceCredentialDigest(service.deviceKeys.Current, service.origin, service.instanceID, accountID, deviceID, credential)
	return authority.DeviceBinding{DeviceID: deviceID, AccountID: accountID, CanonicalOrigin: service.origin,
		CredentialKeyID: digest.KeyID, CredentialDigest: digest.Sum}, credential, nil
}

// resolveExistingDevice proves claim against accountID's active device: the
// credential's digest under the device's stored continuity key, origin and
// instance must equal the stored digest. Any mismatch is
// device_continuity_invalid. A continuity proof only rebinds a device to a
// fresh Apple sign-in or a refresh; it never authenticates by itself.
func (service *Service) resolveExistingDevice(ctx context.Context, accountID string, claim *identity.DeviceContinuityClaim) (authority.DeviceBinding, error) {
	if service == nil || claim == nil || !validUUID(claim.DeviceID) || !validContinuityCredential(claim.Credential) || !validUUID(accountID) {
		return authority.DeviceBinding{}, ErrDeviceContinuityInvalid
	}
	stored, status, err := service.authority.Device(ctx, accountID, claim.DeviceID)
	if err != nil {
		return authority.DeviceBinding{}, err
	}
	if status != "active" || !service.continuityMatches(stored, claim) {
		return authority.DeviceBinding{}, ErrDeviceContinuityInvalid
	}
	return stored, nil
}

// continuityMatches checks claim against a stored device binding: the
// device, this node's origin, and exactly one configured continuity key
// named by the binding whose digest of the credential equals the stored one.
func (service *Service) continuityMatches(stored authority.DeviceBinding, claim *identity.DeviceContinuityClaim) bool {
	if claim == nil || !validUUID(claim.DeviceID) || !validContinuityCredential(claim.Credential) || stored.DeviceID != claim.DeviceID ||
		stored.CanonicalOrigin != service.origin {
		return false
	}
	matches := 0
	for _, key := range append([]identity.DigestKey{service.deviceKeys.Current}, service.deviceKeys.Previous...) {
		candidate := deviceCredentialDigest(key, service.origin, service.instanceID, stored.AccountID, claim.DeviceID, claim.Credential)
		keyMatch := subtle.ConstantTimeCompare([]byte(candidate.KeyID), []byte(stored.CredentialKeyID))
		digestMatch := subtle.ConstantTimeCompare(candidate.Sum[:], stored.CredentialDigest[:])
		matches += keyMatch & digestMatch
	}
	return matches == 1
}

func validContinuityCredential(raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == raw
}
